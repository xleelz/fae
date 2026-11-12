package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// MessageParams records an operator message or reply. The daemon assigns the
// id unless the caller supplies one. A controller may only relay verbatim
// operator words, naming their source; a relay never carries authority.
type MessageParams struct {
	ID      string `json:"id,omitempty"`
	Text    string `json:"text"`
	Task    string `json:"task,omitempty"`
	ReplyTo string `json:"reply_to,omitempty"`
	Relayed bool   `json:"relayed,omitempty"`
	Source  string `json:"source,omitempty"`
}
type InboxParams struct {
	Task      string   `json:"task,omitempty"`
	Kinds     []string `json:"kinds,omitempty"`
	Limit     int      `json:"limit,omitempty"`
	PerThread int      `json:"per_thread,omitempty"`
}
type InboxTask struct {
	Task       string `json:"task"`
	State      string `json:"state"`
	Stage      string `json:"stage,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Dispatch   string `json:"dispatch,omitempty"`
	Workflow   string `json:"workflow"`
	Revision   int64  `json:"revision"`
	BlockedVia string `json:"remediation_route,omitempty"`
	// Decision is the question and options of an undisposed HumanDecision.
	Decision *engine.DecisionRequest `json:"decision,omitempty"`
}
type InboxQuestion struct {
	Task      string                    `json:"task,omitempty"`
	Reason    string                    `json:"reason"`
	Proposals []string                  `json:"proposals,omitempty"`
	Entry     controlplane.ProphetEntry `json:"entry"`
}

var inboxKinds = []string{"threads", "escalated", "blocked", "uncertain", "decisions", "questions"}

func (d *Daemon) messageRPC(ctx context.Context, principal api.Principal, raw json.RawMessage) (any, error) {
	var p MessageParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if principal.Task != "" || (principal.Actor != "operator" && principal.Actor != "controller") {
		return nil, errors.New("only the operator records messages")
	}
	controller := principal.Actor == "controller"
	if p.Relayed != controller || (controller && strings.TrimSpace(p.Source) == "") || (!controller && p.Source != "") {
		return nil, errors.New("a controller may record a message only as a relay of the operator's verbatim words with its source; the operator records directly")
	}
	if p.ID == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		p.ID = "msg-" + hex.EncodeToString(b)
	}
	m, err := d.store.PostMessage(ctx, controlplane.MessageInput{ID: p.ID, Text: p.Text, Task: p.Task, ReplyTo: p.ReplyTo, RecordedBy: principal.Actor, Source: p.Source})
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": m.ID, "thread_root": m.ThreadRoot, "reply_to": m.ReplyTo, "task": m.Task, "relayed": m.Relayed, "author": m.Author, "created_at": m.Created}, nil
}

// inbox is served from the instance plane only: open job snapshots, durable
// Prophet chat pages and recorded operator messages, never the drive cache.
func (d *Daemon) inbox(ctx context.Context, raw json.RawMessage) (any, error) {
	var p InboxParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Limit == 0 {
		p.Limit = 50
	}
	if p.PerThread == 0 {
		p.PerThread = 20
	}
	want := map[string]bool{}
	for _, k := range p.Kinds {
		if !operatorListHas(inboxKinds, k) {
			return nil, errors.New("inbox kinds are threads, escalated, blocked, uncertain, decisions and questions")
		}
		want[k] = true
	}
	if len(want) == 0 {
		for _, k := range inboxKinds {
			want[k] = true
		}
	}
	out := map[string]any{"source": "plane"}
	if want["threads"] {
		threads, err := d.store.MessageThreads(ctx, p.Task, p.Limit, p.PerThread, 4096)
		if err != nil {
			return nil, err
		}
		out["threads"] = threads
	}
	jobs, err := d.store.ListOpenJobs(ctx, 100000)
	if err != nil {
		return nil, err
	}
	lists := map[string][]InboxTask{"escalated": {}, "blocked": {}, "uncertain": {}, "decisions": {}}
	questions := []InboxQuestion{}
	scopes := []string{}
	if p.Task == "" {
		scopes = append(scopes, "")
	}
	for _, s := range jobs {
		if p.Task != "" && s.ID != p.Task {
			continue
		}
		scopes = append(scopes, s.ID)
		var e envelope
		if err := decodeEnvelope(s.Payload, &e); err != nil {
			return nil, err
		}
		j := e.Job
		item := InboxTask{Task: s.ID, State: string(j.State), Reason: j.Reason, Workflow: s.WorkflowID, Revision: s.WorkflowRevision}
		if j.InFlight != nil {
			item.Stage, item.Dispatch = j.InFlight.Stage, j.InFlight.ID
		}
		switch j.State {
		case domain.Escalated:
			lists["escalated"] = append(lists["escalated"], item)
		case domain.Blocked:
			item.Stage, item.BlockedVia = j.BlockedFrom, j.BlockedRoute
			lists["blocked"] = append(lists["blocked"], item)
		}
		if j.Parked && j.Reason == "UncertainDispatch" {
			lists["uncertain"] = append(lists["uncertain"], item)
		}
		for _, request := range engine.PendingDecisions(j) {
			entry := item
			copy := request
			entry.Stage, entry.Decision = request.Stage, &copy
			lists["decisions"] = append(lists["decisions"], entry)
		}
	}
	for k, v := range lists {
		if want[k] {
			out[k] = v
		}
	}
	if want["questions"] {
		for _, scope := range scopes {
			head, err := d.store.ProphetHead(ctx, scope)
			if err != nil {
				return nil, err
			}
			if head == 0 {
				continue
			}
			page, err := d.store.ProphetPage(ctx, scope, head-1, 1)
			if err != nil {
				return nil, err
			}
			if q, ok := unansweredQuestion(scope, page); ok {
				questions = append(questions, q)
			}
		}
		out["questions"] = questions
	}
	return out, nil
}

// A Prophet question is the scope's latest entry: an answered Prophet reply
// that proposes verbs or asks the operator something, with no later operator turn.
func unansweredQuestion(scope string, page []controlplane.ProphetEntry) (InboxQuestion, bool) {
	if len(page) != 1 || page[0].Author != "prophet" {
		return InboxQuestion{}, false
	}
	var detail struct {
		Outcome string        `json:"outcome"`
		Answer  ProphetAnswer `json:"answer"`
	}
	if json.Unmarshal(page[0].Detail, &detail) != nil || detail.Outcome != "answered" {
		return InboxQuestion{}, false
	}
	q := InboxQuestion{Task: scope, Proposals: detail.Answer.Proposals, Entry: page[0]}
	switch {
	case len(detail.Answer.Proposals) > 0:
		q.Reason = "proposals awaiting operator decision"
	case strings.HasSuffix(strings.TrimSpace(page[0].Text), "?"):
		q.Reason = "question awaiting operator answer"
	default:
		return InboxQuestion{}, false
	}
	return q, true
}

func operatorListHas(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

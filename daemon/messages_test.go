package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

func decodeResult(t *testing.T, r api.Response, e error, out any) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(r.Result, out); e != nil {
		t.Fatal(e)
	}
}

// plantJob rewrites a held task snapshot on the plane to reach states the
// stopped drive never produces; only plane reads observe it.
func plantJob(t *testing.T, d *Daemon, id string, edit func(*engine.Job)) {
	t.Helper()
	ctx := context.Background()
	s, e := d.store.GetJob(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	var env envelope
	if e = decodeEnvelope(s.Payload, &env); e != nil {
		t.Fatal(e)
	}
	from := env.Job.State
	edit(&env.Job)
	if env.Job.State != from {
		// The plane refuses a state change no transition records (FAE-WF-022).
		env.Job.History = append(env.Job.History, engine.Event{Kind: "transition", Stage: string(from), From: from, To: env.Job.State, Attempt: 1, Cause: "planted"})
	}
	s.Payload, _ = json.Marshal(env)
	s.State = string(env.Job.State)
	if _, e = d.store.SaveJob(ctx, controlplane.Audit{Actor: "test", Action: "plant inbox state", Outcome: "ok"}, s, s.Version); e != nil {
		t.Fatal(e)
	}
}

func TestMessageThreadsRelayAndInboxNative(t *testing.T) {
	cfg, ctx := fixture(t)
	controllerPath := filepath.Join(filepath.Dir(cfg.Root), "controller-token")
	configPath := filepath.Join(filepath.Dir(cfg.Root), "principals.json")
	raw, _ := json.Marshal([]PrincipalCredential{{CredentialFile: controllerPath, Actor: "controller"}})
	if e := os.WriteFile(configPath, raw, 0600); e != nil {
		t.Fatal(e)
	}
	cfg.PrincipalConfig = configPath
	cfg.ProphetRunner = func(ctx context.Context, task string, brief json.RawMessage) (json.RawMessage, error) {
		a := ProphetAnswer{Reply: "Noted.", Proposals: []string{}}
		if strings.Contains(string(brief), `"message":"PROPOSE`) {
			a.Reply, a.Proposals = "Shall I restart it?", []string{"RestartWithInformation"}
		}
		return json.Marshal(a)
	}
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	for _, id := range []string{"alpha", "beta", "gamma", "delta"} {
		if _, e = call(t, d, "Admit", AdmitParams{ID: id, Content: id, Tree: "tree", AuthorizingMessage: "message"}, id); e != nil {
			t.Fatal(e)
		}
	}
	controller := func(method string, params any) (api.Response, error) {
		token, e := os.ReadFile(controllerPath)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(params)
		return api.Call(ctx, d.layout.Socket, token, api.Request{Version: 1, Method: method, Params: raw})
	}

	// The daemon assigns the id; a root is its own thread.
	var root struct {
		ID, ThreadRoot, Task, Author string
		Relayed                      bool
	}
	r, e := call(t, d, "Message", MessageParams{Text: "Please look at alpha", Task: "alpha"}, "")
	decodeResult(t, r, e, &root)
	if !strings.HasPrefix(root.ID, "msg-") || len(root.ID) != 36 || root.Task != "alpha" || root.Author != "operator" || root.Relayed {
		t.Fatalf("root %+v", root)
	}
	var thread struct {
		ThreadRoot string `json:"thread_root"`
		Task       string `json:"task"`
	}
	if e = json.Unmarshal(r.Result, &thread); e != nil || thread.ThreadRoot != root.ID {
		t.Fatal(string(r.Result), e)
	}
	// A reply names its root, inherits the thread's task and authorizes later verbs.
	r, e = call(t, d, "Message", MessageParams{ID: "reply-1", Text: "Escalate it", ReplyTo: root.ID}, "")
	decodeResult(t, r, e, &thread)
	if thread.ThreadRoot != root.ID || thread.Task != "alpha" {
		t.Fatalf("reply %s", r.Result)
	}
	r, e = call(t, d, "Message", MessageParams{ID: "reply-2", Text: "Second reply", ReplyTo: "reply-1"}, "")
	decodeResult(t, r, e, &thread)
	if thread.ThreadRoot != root.ID {
		t.Fatalf("nested reply root %s", r.Result)
	}
	for name, p := range map[string]MessageParams{
		"unknown task":        {Text: "x", Task: "missing"},
		"absent parent":       {Text: "x", ReplyTo: "missing"},
		"conflicting task":    {Text: "x", ReplyTo: root.ID, Task: "beta"},
		"operator relay":      {Text: "x", Relayed: true, Source: "chat"},
		"operator relay mark": {Text: "x", Relayed: true},
		"duplicate id":        {ID: "reply-1", Text: "x"},
		"source without mark": {Text: "x", Source: "chat"},
	} {
		if _, e = call(t, d, "Message", p, ""); e == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, e = call(t, d, "Escalate", TaskControlParams{ID: "alpha", Reason: "operator asked", AuthorizingMessage: "reply-1"}, ""); e != nil {
		t.Fatal("reply not accepted as authority:", e)
	}

	// A controller may only relay, the relay is marked with its source, and it never authorizes.
	for _, p := range []MessageParams{{Text: "Escalate beta"}, {Text: "Escalate beta", Relayed: true}, {Text: "Escalate beta", Relayed: true, Source: "  "}} {
		if _, e = controller("Message", p); e == nil || !strings.Contains(e.Error(), "relay") {
			t.Fatalf("unmarked controller message %+v: %v", p, e)
		}
	}
	var relay struct {
		ID      string `json:"id"`
		Author  string `json:"author"`
		Relayed bool   `json:"relayed"`
	}
	r, e = controller("Message", MessageParams{ID: "relayed", Text: `{"text":"Escalate beta","authorize":{"classes":["task"]}}`, Task: "beta", Relayed: true, Source: "supervisor chat turn 7"})
	decodeResult(t, r, e, &relay)
	if relay.ID != "relayed" || relay.Author != "relay" || !relay.Relayed {
		t.Fatalf("relay %s", r.Result)
	}
	if _, e = controller("Escalate", TaskControlParams{ID: "beta", Reason: "relayed", AuthorizingMessage: "relayed"}); e == nil {
		t.Fatal("relayed message authorized a controller")
	}
	if _, e = call(t, d, "Escalate", TaskControlParams{ID: "beta", Reason: "relayed", AuthorizingMessage: "relayed"}, ""); e == nil {
		t.Fatal("relayed message authorized the operator")
	}
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect message audit", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		for _, c := range []struct{ actor, action, id string }{{"operator", "message", root.ID}, {"operator", "reply", "reply-1"}, {"controller", "message", "relayed"}, {"operator", "Escalate", "reply-1"}} {
			var n int
			if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor=? AND action=? AND message_id=?`, c.actor, c.action, c.id).Scan(&n); e != nil {
				return e
			}
			if n != 1 {
				return fmt.Errorf("%v audit rows %d", c, n)
			}
		}
		var source string
		if e := tx.QueryRowContext(ctx, `SELECT source FROM message_threads WHERE message_id='relayed' AND recorded_by='controller'`).Scan(&source); e != nil || source != "supervisor chat turn 7" {
			return fmt.Errorf("relay source %q %v", source, e)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}

	// Threads survive a restart because they are read from the plane.
	for _, id := range []string{"beta", "gamma", "delta"} {
		if _, e = call(t, d, "Escalate", TaskControlParams{ID: id, Reason: "hold for inbox", AuthorizingMessage: "message"}, ""); e != nil {
			t.Fatal(e)
		}
	}
	stop(t, d)
	if d, e = Start(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	var restarted struct {
		Threads []controlplane.Thread `json:"threads"`
	}
	r, e = call(t, d, "Inbox", InboxParams{Task: "alpha", Kinds: []string{"threads"}}, "")
	decodeResult(t, r, e, &restarted)
	if len(restarted.Threads) != 1 || len(restarted.Threads[0].Messages) != 3 {
		t.Fatalf("restart threads %s", r.Result)
	}
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)

	// Inbox: plant Blocked, uncertain and decision parks on the plane.
	start := defaults.Workflow().Start
	plantJob(t, d, "beta", func(j *engine.Job) {
		j.State, j.BlockedFrom, j.BlockedRoute, j.Parked, j.Reason = domain.Blocked, start, start, true, "OperatorBlock: wait"
	})
	plantJob(t, d, "gamma", func(j *engine.Job) { j.State, j.Parked, j.Reason = domain.State(start), true, "UncertainDispatch" })
	plantJob(t, d, "delta", func(j *engine.Job) { j.State, j.Parked, j.Reason = domain.State(start), true, "HumanDecisionRequired" })
	question := prophetSend(t, d, ProphetParams{Task: "gamma", Text: "PROPOSE something"})
	prophetAwait(t, d, "gamma", question)
	answered := prophetSend(t, d, ProphetParams{Task: "delta", Text: "PROPOSE then answer"})
	prophetAwait(t, d, "delta", answered)
	closed := prophetSend(t, d, ProphetParams{Task: "delta", Text: "thanks"})
	prophetAwait(t, d, "delta", closed)

	var inbox struct {
		Source    string                `json:"source"`
		Threads   []controlplane.Thread `json:"threads"`
		Escalated []InboxTask           `json:"escalated"`
		Blocked   []InboxTask           `json:"blocked"`
		Uncertain []InboxTask           `json:"uncertain"`
		Decisions []InboxTask           `json:"decisions"`
		Questions []InboxQuestion       `json:"questions"`
	}
	r, e = call(t, d, "Inbox", InboxParams{}, "")
	decodeResult(t, r, e, &inbox)
	if inbox.Source != "plane" || len(inbox.Escalated) != 1 || inbox.Escalated[0].Task != "alpha" || len(inbox.Blocked) != 1 || inbox.Blocked[0].Task != "beta" || inbox.Blocked[0].BlockedVia != start || len(inbox.Uncertain) != 1 || inbox.Uncertain[0].Task != "gamma" || len(inbox.Decisions) != 1 || inbox.Decisions[0].Task != "delta" {
		t.Fatalf("inbox tasks %s", r.Result)
	}
	if len(inbox.Questions) != 1 || inbox.Questions[0].Task != "gamma" || len(inbox.Questions[0].Proposals) != 1 {
		t.Fatalf("inbox questions %+v", inbox.Questions)
	}
	var alphaThread *controlplane.Thread
	for i := range inbox.Threads {
		if strings.HasPrefix(inbox.Threads[i].Root, "prophet-") {
			t.Fatal("Prophet chat leaked into operator threads")
		}
		if inbox.Threads[i].Root == root.ID {
			alphaThread = &inbox.Threads[i]
		}
	}
	if alphaThread == nil || alphaThread.Task != "alpha" || len(alphaThread.Messages) != 3 || alphaThread.Messages[1].ReplyTo != root.ID || alphaThread.Messages[2].ReplyTo != "reply-1" || alphaThread.Messages[0].Text != "Please look at alpha" {
		t.Fatalf("thread %+v", alphaThread)
	}
	r, e = call(t, d, "Inbox", InboxParams{Task: "beta", Kinds: []string{"threads", "blocked"}}, "")
	var filtered map[string]json.RawMessage
	decodeResult(t, r, e, &filtered)
	var threads []controlplane.Thread
	if e = json.Unmarshal(filtered["threads"], &threads); e != nil || len(threads) != 1 || threads[0].Root != "relayed" || !threads[0].Messages[0].Relayed || threads[0].Messages[0].Source != "supervisor chat turn 7" {
		t.Fatalf("filtered threads %s", filtered["threads"])
	}
	if _, ok := filtered["escalated"]; ok {
		t.Fatal("kind filter ignored")
	}
	if _, e = call(t, d, "Inbox", InboxParams{Kinds: []string{"bogus"}}, ""); e == nil {
		t.Fatal("unknown inbox kind accepted")
	}
}

package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
)

type ProphetParams struct {
	ActionTask string   `json:"action_task,omitempty"`
	Task       string   `json:"task,omitempty"`
	Text       string   `json:"text,omitempty"`
	After      int64    `json:"after,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	Direct     []string `json:"direct,omitempty"`
}
type ProphetAnswer struct {
	Reply       string   `json:"reply"`
	Proposals   []string `json:"proposals"`
	Action      string   `json:"action"`
	Task        string   `json:"task"`
	Information string   `json:"information"`
	Context     string   `json:"context"`
}

const prophetSchema = `{"type":"object","properties":{"reply":{"type":"string"},"proposals":{"type":"array","items":{"type":"string"}},"action":{"type":"string"},"task":{"type":"string"},"information":{"type":"string"},"context":{"type":"string"}},"required":["reply","proposals","action","task","information","context"],"additionalProperties":false}`

func (d *Daemon) prophetRead(ctx context.Context, p ProphetParams) (any, error) {
	if p.Limit == 0 {
		p.Limit = 50
	}
	page, err := d.store.ProphetPage(ctx, p.Task, p.After, p.Limit)
	if err != nil {
		return nil, err
	}
	states := map[string]string{}
	for _, entry := range page {
		if entry.Author != "operator" {
			continue
		}
		raw, e := d.store.GetMarker(ctx, "prophet-turn/"+entry.ID)
		if e != nil {
			return nil, e
		}
		var state struct {
			Status  string    `json:"status"`
			Started time.Time `json:"daemon_started"`
		}
		if e = json.Unmarshal(raw, &state); e != nil {
			return nil, e
		}
		if state.Status == "queued" && !state.Started.Equal(d.started) {
			state.Status = "interrupted; no automatic replay"
		}
		states[entry.ID] = state.Status
	}
	var scope any
	if p.Task != "" {
		j, e := d.store.GetJob(ctx, p.Task)
		if e != nil {
			return nil, e
		}
		var env envelope
		if e = decodeEnvelope(j.Payload, &env); e != nil {
			return nil, e
		}
		historyStart := max(0, len(env.Job.History)-100)
		evidenceStart := max(0, len(env.Job.Evidence)-100)
		contextStart := max(0, len(env.Job.ContextEntries)-100)
		scope = map[string]any{"task": p.Task, "content": boundedProphetText(env.Admission.Content, 65536), "content_bytes": len(env.Admission.Content), "state": env.Job.State, "history": env.Job.History[historyStart:], "history_start": historyStart, "history_total": len(env.Job.History), "artifacts": env.Job.Evidence[evidenceStart:], "artifacts_start": evidenceStart, "artifacts_total": len(env.Job.Evidence), "operator_context": env.Job.ContextEntries[contextStart:], "workflow": env.Job.Document, "disposition": env.Job.Disposition}
	} else {
		jobs, e := d.store.ListJobs(ctx, 1000)
		if e != nil {
			return nil, e
		}
		summaries := []map[string]any{}
		for _, j := range jobs {
			var env envelope
			if err := decodeEnvelope(j.Payload, &env); err != nil {
				return nil, err
			}
			summaries = append(summaries, map[string]any{"task": j.ID, "state": j.State, "workflow": j.WorkflowID, "revision": j.WorkflowRevision, "content_excerpt": boundedProphetText(env.Admission.Content, 1024), "reason": boundedProphetText(env.Job.Reason, 1024), "disposition": env.Job.Disposition, "history_events": len(env.Job.History)})
		}
		metadata, e := d.store.ProphetGlobalMetadata(ctx)
		if e != nil {
			return nil, e
		}
		scope = map[string]any{"tasks": summaries, "health": d.statusSummary(), "plane": metadata}
	}
	next := p.After
	if len(page) > 0 {
		next = page[len(page)-1].Sequence
	}
	result := map[string]any{"scope": scope, "messages": page, "next_after": next, "turns": states}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if len(raw) > 2<<20 {
		return nil, errors.New("Prophet page exceeds 2 MiB; reduce page size and inspect TaskHistory separately")
	}
	return result, nil
}
func (d *Daemon) prophetRPC(ctx context.Context, r api.Request) (any, error) {
	var p ProphetParams
	if err := decode(r.Params, &p); err != nil {
		return nil, err
	}
	if r.Method == "ProphetSession" || r.Method == "ProphetChatRead" {
		return d.prophetRead(ctx, p)
	}
	if len(p.Text) == 0 || len(p.Text) > 65536 {
		return nil, errors.New("Prophet text must contain 1..65536 bytes")
	}
	if len(p.Direct) > 0 && p.Task == "" && p.ActionTask == "" {
		return nil, errors.New("global direction requires explicit action_task")
	}
	if p.Task != "" && p.ActionTask != "" && p.ActionTask != p.Task {
		return nil, errors.New("direction scope mismatch")
	}
	for _, v := range p.Direct {
		if v != "RestartWithInformation" && v != "ResumeWithContext" {
			return nil, errors.New("unsupported explicit Prophet direction")
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() != nil || d.draining || d.backlogUncertain {
		return nil, errors.New("daemon is draining")
	}
	if _, err := os.Stat(filepath.Join(d.layout.Root, "maintenance")); err == nil {
		return nil, errors.New("maintenance")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if d.prophetActive == nil {
		d.prophetActive = map[string]bool{}
	}
	if d.prophetActive[p.Task] || len(d.prophetActive) >= 8 {
		return nil, errors.New("Prophet session busy")
	}
	if d.live == nil && d.config.ProphetRunner == nil {
		return nil, errors.New("Prophet provider not configured")
	}
	head, err := d.store.ProphetHead(ctx, p.Task)
	if err != nil {
		return nil, err
	}
	scope, err := d.prophetRead(ctx, ProphetParams{Task: p.Task, After: max(0, head-100), Limit: 100})
	if err != nil {
		return nil, err
	}
	rawScope, err := json.Marshal(scope)
	if err != nil || len(rawScope) > 1<<20 {
		return nil, errors.New("Prophet context exceeds bounded turn; page history separately")
	}
	idBytes := make([]byte, 16)
	if _, err = rand.Read(idBytes); err != nil {
		return nil, err
	}
	id := "prophet-" + hex.EncodeToString(idBytes)
	message, _ := json.Marshal(map[string]any{"text": p.Text, "authorize": controlplane.Direction{Verbs: p.Direct}, "task": p.Task, "action_task": p.ActionTask})
	if err = d.store.RecordMessage(ctx, id, string(message)); err != nil {
		return nil, err
	}
	intent, _ := json.Marshal(map[string]any{"status": "queued", "daemon_started": d.started, "task": p.Task})
	if err = d.store.PutMarker(ctx, controlplane.Audit{Actor: "operator", Action: "ProphetTurnQueued", MessageID: id, Outcome: "queued"}, "prophet-turn/"+id, intent); err != nil {
		return nil, err
	}
	requestDetail, _ := json.Marshal(map[string]any{"direct": p.Direct, "action_task": p.ActionTask})
	entry, err := d.store.AppendProphet(ctx, controlplane.Audit{Actor: "operator", Action: "ProphetChat", MessageID: id, Outcome: "queued"}, controlplane.ProphetEntry{ID: id, Task: p.Task, Author: "operator", Text: p.Text, Detail: requestDetail})
	if err != nil {
		return nil, err
	}
	brief, _ := json.Marshal([]any{map[string]any{"kind": "kernel_contract", "output_schema": json.RawMessage(prophetSchema)}, map[string]any{"kind": "stage", "stage": "prophet"}, map[string]any{"kind": "context", "scope": json.RawMessage(rawScope), "message": p.Text, "directed_verbs": p.Direct, "action_task": p.ActionTask, "instructions": "Answer and propose only. Never claim quality approval. action may be empty, RestartWithInformation, or ResumeWithContext; enact only a directed verb and exact task."}})
	d.prophetActive[p.Task] = true
	d.workers.Add(1)
	d.publishStatus()
	go d.prophetTurn(p, id, brief)
	return map[string]any{"id": id, "queued": true, "entry": entry}, nil
}
func (d *Daemon) prophetTurn(p ProphetParams, id string, brief json.RawMessage) {
	defer d.workers.Done()
	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Minute)
	defer cancel()
	var raw json.RawMessage
	var err error
	if d.config.ProphetRunner != nil {
		raw, err = d.config.ProphetRunner(ctx, p.Task, brief)
	} else {
		raw, err = d.runProphet(ctx, p.Task, id, brief)
	}
	var answer ProphetAnswer
	if err == nil {
		err = decode(raw, &answer)
		if len(raw) > 1<<20 || answer.Reply == "" || len(answer.Reply) > 65536 || len(answer.Proposals) > 32 || len(answer.Information) > 65536 || len(answer.Context) > 65536 {
			err = errors.New("invalid bounded Prophet response")
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	defer func() { delete(d.prophetActive, p.Task); d.publishStatus() }()
	outcome := "answered"
	text := answer.Reply
	if err == nil && answer.Action != "" && (d.ctx.Err() != nil || d.draining || d.backlogUncertain) {
		err = errors.New("Prophet action refused while draining, shutting down, or checkpoint recovery")
	}
	if err == nil && answer.Action != "" {
		directed := false
		for _, v := range p.Direct {
			directed = directed || v == answer.Action
		}
		supported := (answer.Action == "RestartWithInformation" || answer.Action == "ResumeWithContext") && answer.Task != "" && ((p.Task != "" && answer.Task == p.Task) || (p.Task == "" && answer.Task == p.ActionTask))
		if !CanEnactProphet(directed, supported) {
			err = errors.New("Prophet action refused: exact operator direction and task required")
		} else {
			var actionCtx context.Context
			actionCtx, err = d.store.AuthorizeAction(ctx, id, "prophet", answer.Action, "task", false, actionClasses())
			if err == nil {
				params, _ := json.Marshal(TaskControlParams{ID: answer.Task, Information: answer.Information, Context: answer.Context, AuthorizingMessage: id})
				_, err = d.taskControl(actionCtx, answer.Action, params, controlplane.Audit{Actor: "prophet", Action: answer.Action, MessageID: id, Inputs: params, Outcome: "ok"})
			}
		}
	}
	if err != nil {
		outcome = "failed"
		text = fmt.Sprintf("Prophet turn failed: %v", err)
	}
	cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	detail, _ := json.Marshal(map[string]any{"outcome": outcome, "answer": answer})
	_, saveErr := d.store.AppendProphet(cleanup, controlplane.Audit{Actor: "prophet", Action: "ProphetReply", MessageID: id, Outcome: outcome}, controlplane.ProphetEntry{ID: id + "-reply", Task: p.Task, Author: "prophet", Text: text, ReplyTo: id, Detail: detail})
	if saveErr != nil && d.log != nil {
		fmt.Fprintf(d.log, "Prophet reply checkpoint failed %s: %v\n", id, saveErr)
	}
	state, _ := json.Marshal(map[string]any{"status": outcome, "daemon_started": d.started, "task": p.Task})
	if saveErr == nil {
		_ = d.store.PutMarker(cleanup, controlplane.Audit{Actor: "prophet", Action: "ProphetTurnCompleted", MessageID: id, Outcome: outcome}, "prophet-turn/"+id, state)
	}
	d.publishStatus()
}

func prophetModel(c Config) string {
	if c.ProphetModel != "" {
		return c.ProphetModel
	}
	return "gpt-6-astra"
}

func boundedProphetText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + " [truncated; inspect task history]"
}

func prophetProvider(c Config) string {
	if c.ProphetProvider != "" {
		return c.ProphetProvider
	}
	return "codex"
}

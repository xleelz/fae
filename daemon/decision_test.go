package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// decisionWorkflow is a small protected graph with a HumanDecision between
// the independent review and the commit.
func decisionWorkflow() workflow.Document {
	role := workflow.Role{Instructions: "instructions", Briefs: map[string]string{"default": "brief"}, Routes: []workflow.Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 2, Outputs: []string{"report"}}
	field := func(path string) workflow.Expr {
		return workflow.Expr{Op: "field", Stage: "test", Path: []string{path}}
	}
	literal := func(v any) workflow.Expr { return workflow.Expr{Op: "literal", Value: v} }
	gate := workflow.Expr{Op: "and", Args: []workflow.Expr{{Op: "eq", Args: []workflow.Expr{field("exit_code"), literal(0)}}, {Op: "ge", Args: []workflow.Expr{field("executed_count"), literal(1)}}}}
	return currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "decisions", Start: "change", Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 3},
		Roles: map[string]workflow.Role{"coder": role, "reviewer": role},
		Artifacts: map[string]workflow.ArtifactType{"report": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)},
			"Decision": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["decision_id","option","authorizing_message"]}`)}},
		Stages: map[string]workflow.Stage{
			"change":  {Primitive: workflow.AgentTurn, Role: "coder", Output: "report", ChangeProducer: true},
			"test":    {Primitive: workflow.HostCommand, Output: "report", Command: []string{"go", "test", "-json", "-count=1", "./..."}, Weight: 1, TimeoutSeconds: 30, TestRun: true},
			"gate":    {Primitive: workflow.Gate, Expr: &gate, TestStage: "test", Failure: "stop"},
			"review":  {Primitive: workflow.Review, Role: "reviewer", Output: "report"},
			"approve": {Primitive: workflow.HumanDecision, Question: "Ship the reviewed change?", Options: []workflow.DecisionOption{{ID: "ship", Next: "commit"}, {ID: "rework", Next: "again"}, {ID: "halt", Next: "stop"}}, OnTimeout: workflow.StayWaiting, Output: "Decision"},
			"again":   {Primitive: workflow.Loop, MaxIterations: 2},
			"commit":  {Primitive: workflow.VcsCommit, Output: "report", Protected: true},
			"stop":    {Primitive: workflow.Escalate},
		},
		Edges: []workflow.Edge{{From: "change", To: "test"}, {From: "test", To: "gate"}, {From: "gate", To: "review"}, {From: "gate", To: "stop"}, {From: "review", To: "approve"}, {From: "approve", To: "commit"}, {From: "approve", To: "again"}, {From: "approve", To: "stop"}, {From: "again", To: "change"}, {From: "commit", To: "Done"}},
	})
}
func decisionScript() map[string][]engine.ScriptStep {
	script := map[string][]engine.ScriptStep{"commit": {{Output: json.RawMessage(`{"sha":"scripted"}`)}}}
	for i := 0; i < 3; i++ {
		script["change"] = append(script["change"], engine.ScriptStep{Output: json.RawMessage(fmt.Sprintf(`{"change":%d}`, i))})
		script["test"] = append(script["test"], engine.ScriptStep{Output: json.RawMessage(`{"exit_code":0,"executed_count":1,"fresh_build":true}`)})
		script["review"] = append(script["review"], engine.ScriptStep{Reviews: []json.RawMessage{json.RawMessage(`{"approved":true}`)}})
	}
	return script
}

// awaitDecision waits until the task is parked on the given decision visit.
func awaitDecision(t *testing.T, d *Daemon, id string, visit int) engine.DecisionRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if s, err := d.store.GetJob(context.Background(), id); err == nil {
			var e envelope
			if decodeEnvelope(s.Payload, &e) == nil {
				if req, ok := engine.PendingDecision(e.Job); ok && req.Visit == visit {
					return req
				}
			}
		}
		select {
		case <-d.done:
			t.Fatalf("daemon stopped: %v", d.err)
		default:
		}
		time.Sleep(30 * time.Millisecond)
	}
	s, err := d.store.GetJob(context.Background(), id)
	tail := string(s.Payload)
	if n := strings.Index(tail, `"history"`); n > 0 {
		tail = tail[n:]
	}
	status, _ := call(t, d, "Status", map[string]any{}, "")
	t.Fatalf("task %s never waited for decision visit %d: %s %v %s\nstatus %s", id, visit, s.State, err, tail[:min(len(tail), 200)], status.Result)
	return engine.DecisionRequest{}
}

// FAE-WF-030 HumanDecision end to end through the daemon: the inbox lists the
// question and options, the decide verb is authorized and audited, the
// decision row commits with the transition, a waiting decision survives a
// restart, and duplicate or cancelled decisions are refused.
func TestDecideVerbEndToEnd(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(decisionScript())
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := decisionWorkflow()
	if _, e = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"alpha", "beta"} {
		if _, e = call(t, d, "Admit", AdmitParams{ID: id, WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, id); e != nil {
			t.Fatal(e)
		}
	}
	first := awaitDecision(t, d, "alpha", 1)
	awaitDecision(t, d, "beta", 1)
	var inbox struct {
		Decisions []InboxTask `json:"decisions"`
	}
	r, e := call(t, d, "Inbox", InboxParams{Kinds: []string{"decisions"}}, "")
	decodeResult(t, r, e, &inbox)
	if len(inbox.Decisions) != 2 {
		t.Fatal("inbox decisions", string(r.Result))
	}
	for _, item := range inbox.Decisions {
		if item.Stage != "approve" || item.Decision == nil || item.Decision.Question != "Ship the reviewed change?" || len(item.Decision.Options) != 3 {
			t.Fatal("inbox decision prompt", item)
		}
	}
	for name, p := range map[string]DecideParams{
		"no authority":   {ID: "alpha", Option: "ship"},
		"unknown task":   {ID: "missing", Option: "ship", AuthorizingMessage: "message"},
		"no option":      {ID: "alpha", AuthorizingMessage: "message"},
		"unknown option": {ID: "alpha", Option: "merge", AuthorizingMessage: "message"},
		"stale decision": {ID: "alpha", Option: "ship", Decision: "decision-0", AuthorizingMessage: "message"},
	} {
		if _, e = call(t, d, "Decide", p, ""); e == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	var decided struct {
		DecisionID string `json:"decision_id"`
		Option     string `json:"option"`
		Next       string `json:"next"`
		ArtifactID string `json:"artifact_id"`
	}
	r, e = call(t, d, "Decide", DecideParams{ID: "alpha", Option: "rework", Decision: first.ID, Rationale: "tighten it", AuthorizingMessage: "message"}, "")
	decodeResult(t, r, e, &decided)
	if decided.DecisionID != first.ID || decided.Option != "rework" || decided.Next != "again" || decided.ArtifactID == "" {
		t.Fatal(string(r.Result))
	}
	if _, e = call(t, d, "Decide", DecideParams{ID: "alpha", Option: "ship", Decision: first.ID, AuthorizingMessage: "message"}, ""); e == nil {
		t.Fatal("duplicate decision accepted")
	}
	second := awaitDecision(t, d, "alpha", 2)
	// A waiting decision survives a restart; nothing decides it meanwhile.
	stop(t, d)
	if d, e = Start(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	if again := awaitDecision(t, d, "alpha", 2); again.ID != second.ID {
		t.Fatal("restart changed the pending decision", again.ID, second.ID)
	}
	if _, e = call(t, d, "Decide", DecideParams{ID: "alpha", Option: "ship", Decision: second.ID, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	awaitState(t, d, "alpha", "Done")
	// Cancellation: a blocked, then cancelled, task can no longer be decided.
	for _, method := range []string{"Block", "Cancel"} {
		if _, e = call(t, d, method, TaskControlParams{ID: "beta", Reason: "not needed", AuthorizingMessage: "message"}, ""); e != nil {
			t.Fatal(method, e)
		}
	}
	if _, e = call(t, d, "Decide", DecideParams{ID: "beta", Option: "ship", AuthorizingMessage: "message"}, ""); e == nil || !strings.Contains(e.Error(), "not waiting") {
		t.Fatal("cancelled task decided", e)
	}
	// Gate results and escalations are decision rows too; only the human
	// decisions are asserted here.
	all, e := d.store.TaskDecisionPayloads(ctx, "alpha", 100)
	rows := all[:0:0]
	for _, r := range all {
		if r.Kind == "HumanDecision" {
			rows = append(rows, r)
		}
	}
	if e != nil || len(rows) != 2 || rows[0].ID != first.ID || rows[1].ID != second.ID || rows[0].Role != "operator" || rows[0].Rationale != "tighten it" || !strings.Contains(string(rows[0].Payload), `"authorizing_message":"message"`) {
		t.Fatal("decision rows", rows, e)
	}
	if beta, e := d.store.TaskDecisionPayloads(ctx, "beta", 100); e != nil || slices.ContainsFunc(beta, func(r controlplane.DecisionRow) bool { return r.Kind == "HumanDecision" }) {
		t.Fatal("refused decision recorded", beta, e)
	}
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect decision audit", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor='operator' AND action='Decide' AND message_id='message'`).Scan(&n); e != nil || n != 2 {
			return fmt.Errorf("decision audit rows %d %v", n, e)
		}
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE kind='decision' AND job_id='alpha'`).Scan(&n); e != nil || n != 2 {
			return fmt.Errorf("decision events %d %v", n, e)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

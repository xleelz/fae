package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// fanWorkflow replaces the decision of decisionWorkflow with two review
// branches joined by an all-pass FanIn.
func fanWorkflow() workflow.Document {
	d := decisionWorkflow()
	d.ID = "fans"
	delete(d.Stages, "approve")
	delete(d.Stages, "again")
	delete(d.Artifacts, "Decision")
	d.Roles["security"] = d.Roles["reviewer"]
	approved := func(stage string) *workflow.Expr {
		x := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "count", Stage: stage, Args: []workflow.Expr{{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: stage, Path: []string{"approved"}}, {Op: "literal", Value: true}}}}}, {Op: "literal", Value: 1}}}
		return &x
	}
	d.Stages["fan"] = workflow.Stage{Primitive: workflow.FanOut, Join: "join", Branches: []workflow.FanBranch{{ID: "correctness", Start: "review", Pass: approved("review")}, {ID: "security", Start: "secure", Pass: approved("secure")}}}
	d.Stages["secure"] = workflow.Stage{Primitive: workflow.Review, Role: "security", Output: "report"}
	d.Stages["join"] = workflow.Stage{Primitive: workflow.FanIn, Rule: workflow.JoinAll, Failure: "stop"}
	d.Edges = []workflow.Edge{{From: "change", To: "test"}, {From: "test", To: "gate"}, {From: "gate", To: "fan"}, {From: "gate", To: "stop"}, {From: "fan", To: "review"}, {From: "fan", To: "secure"}, {From: "review", To: "join"}, {From: "secure", To: "join"}, {From: "join", To: "commit"}, {From: "join", To: "stop"}, {From: "commit", To: "Done"}}
	return currentWorkflowFixture(d)
}

// FAE-WF-030 FanOut/FanIn through the daemon: the durable branch state is
// persisted with the job, a graceful restart while the second branch's turn
// is in flight resumes inside the fan, and the join reaches the commit.
func TestFanOutDaemonRestartInsideBranch(t *testing.T) {
	cfg, ctx := fixture(t)
	script := decisionScript()
	script["secure"] = []engine.ScriptStep{{Reviews: []json.RawMessage{json.RawMessage(`{"approved":true}`)}, Delay: 5 * time.Second}, {Reviews: []json.RawMessage{json.RawMessage(`{"approved":true}`)}}}
	cfg.Executor = engine.NewScripted(script)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := fanWorkflow()
	if _, e = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "Admit", AdmitParams{ID: "alpha", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "alpha"); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		s, err := d.store.GetJob(context.Background(), "alpha")
		var env envelope
		if err == nil && decodeEnvelope(s.Payload, &env) == nil && env.Job.InFlight != nil && env.Job.InFlight.Stage == "secure" {
			if env.Job.Fan == nil || env.Job.Fan.Branches[0].Status != engine.BranchPassed {
				t.Fatal("branch state not persisted", env.Job.Fan)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second branch never dispatched", s.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop(t, d)
	if d, e = Start(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	s := awaitState(t, d, "alpha", "Done")
	var env envelope
	if e = decodeEnvelope(s.Payload, &env); e != nil {
		t.Fatal(e)
	}
	joined := []string{}
	for _, ev := range env.Job.History {
		if ev.Kind == "branch-joined" || ev.Kind == "join" {
			joined = append(joined, ev.Kind+":"+ev.Cause)
		}
	}
	if strings.Join(joined, ",") != "branch-joined:correctness passed,branch-joined:security passed,join:all: 2 passed, 0 failed of 2" || env.Job.Fan != nil {
		t.Fatal(joined)
	}
	if e = engine.ReplayGates(env.Job); e != nil {
		t.Fatal(e)
	}
}

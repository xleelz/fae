package engine_test

import (
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// staleWorkflow: change, test, gate, review, commit, then an Integrate whose
// declared re-verification loop runs the test, gate and review again.
func staleWorkflow(reverify bool) workflow.Document {
	any := json.RawMessage(`{"type":"object"}`)
	pass := workflow.Expr{Op: "and", Args: []workflow.Expr{
		{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "test", Path: []string{"exit_code"}}, {Op: "literal", Value: 0}}},
		{Op: "ge", Args: []workflow.Expr{{Op: "field", Stage: "test", Path: []string{"executed_count"}}, {Op: "literal", Value: 1}}}}}
	role := func() workflow.Role {
		return workflow.Role{Instructions: "fixture", Briefs: map[string]string{"default": "fixture"}, Routes: []workflow.Binding{{Provider: "scripted", Model: "fixture"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"Report"}}
	}
	d := workflow.Document{SchemaVersion: 1, ID: "stale", Start: "change", Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{"coder": role(), "reviewer": role()},
		Artifacts: map[string]workflow.ArtifactType{"Report": {Version: 1, Schema: any}},
		Stages: map[string]workflow.Stage{
			"change":    {Primitive: workflow.AgentTurn, Role: "coder", Output: "Report", ChangeProducer: true},
			"test":      {Primitive: workflow.HostCommand, Output: "Report", Command: []string{"go", "test", "./..."}, Weight: 1, TimeoutSeconds: 30, TestRun: true},
			"gate":      {Primitive: workflow.Gate, Expr: &pass, TestStage: "test", Failure: "stop"},
			"review":    {Primitive: workflow.Review, Role: "reviewer", Output: "Report"},
			"commit":    {Primitive: workflow.VcsCommit, Output: "Report", Protected: true},
			"integrate": {Primitive: workflow.Integrate, Output: "Report", Protected: true},
			"stop":      {Primitive: workflow.Escalate}},
		Edges: edges("change", "test", "test", "gate", "gate", "review", "gate", "stop", "review", "commit", "commit", "integrate", "integrate", "Done")}
	if reverify {
		s := d.Stages["integrate"]
		s.Reverify = "retry"
		d.Stages["integrate"] = s
		d.Stages["retry"] = workflow.Stage{Primitive: workflow.Loop, MaxIterations: 3}
		d.Edges = append(d.Edges, edges("integrate", "retry", "retry", "test")...)
	}
	return d
}

// FAE-ROB-013 acceptance: a verdict for a tree that no longer exists is stale
// and its stage re-runs. A protected stage whose test and review verdicts
// predate the last change re-validates through its declared re-verification
// route; only a stage with no such route escalates ResumeEvidence.
func TestStaleVerdictReruns(t *testing.T) {
	ok := json.RawMessage(`{"exit_code":0,"executed_count":1,"fresh_build":true,"approved":true}`)
	script := map[string][]engine.ScriptStep{}
	for _, stage := range []string{"change", "test", "review", "commit", "integrate"} {
		for i := 0; i < 3; i++ {
			script[stage] = append(script[stage], engine.ScriptStep{Output: ok, Reviews: []json.RawMessage{ok}})
		}
	}
	for _, reverify := range []bool{true, false} {
		doc := staleWorkflow(reverify)
		if issues := workflow.Validate(doc); len(issues) > 0 {
			t.Fatal(issues)
		}
		j, err := engine.New("stale", 1, doc, "tree")
		if err != nil {
			t.Fatal(err)
		}
		j = advanceToWith(t, j, engine.NewScripted(script), "integrate")
		// A reconciliation after the verdicts leaves them stale for this tree.
		var last string
		for _, e := range j.History {
			if e.Kind == "dispatch" {
				last = e.DispatchID
			}
		}
		j.History = append(j.History, engine.Event{Kind: "tree-reconciled", Stage: "commit", DispatchID: last, Attempt: j.Attempts, Cause: "fixture: base moved after the verdicts"})
		next, d, events, err := engine.Prepare(j)
		if err != nil || d != nil {
			t.Fatal(err, d)
		}
		if !reverify {
			if next.EscalationKind != domain.ResumeEvidence {
				t.Fatal("unroutable stale evidence", next.State, next.Reason)
			}
			continue
		}
		if next.State != "retry" || next.EscalationKind != "" || len(events) < 2 || events[0].Kind != "evidence-stale" || events[1].From != "integrate" || events[1].To != "retry" {
			t.Fatal("stale verdict did not re-validate", next.State, next.Reason, events)
		}
		done := drive(t, next, engine.NewScripted(script), false)
		if done.State != domain.Done || done.Counts["test"] != 2 || done.Counts["review"] != 2 || done.Counts["change"] != 1 {
			t.Fatal("stale verdict stages did not re-run", done.State, done.Reason, done.Counts)
		}
	}
}

func edges(pairs ...string) []workflow.Edge {
	out := []workflow.Edge{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, workflow.Edge{From: pairs[i], To: pairs[i+1]})
	}
	return out
}

func advanceToWith(t *testing.T, j engine.Job, executor engine.Executor, stage string) engine.Job {
	t.Helper()
	for i := 0; i < 100 && string(j.State) != stage; i++ {
		next, d, _, err := engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		j = next
		if d == nil {
			continue
		}
		r, err := executor.Execute(t.Context(), *d)
		if err != nil {
			t.Fatal(err)
		}
		if j, _, err = engine.Complete(j, d.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	if string(j.State) != stage {
		t.Fatalf("did not reach %s: %s %s", stage, j.State, j.Reason)
	}
	return j
}

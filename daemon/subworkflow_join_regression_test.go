package daemon

import (
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
)

func subworkflowJoinFixture(fail bool) (workflow.Document, workflow.Scenario) {
	child := childWorkflow()
	child.ID = "join-child"
	ok := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "draft", Path: []string{"ok"}}, {Op: "literal", Value: true}}}
	child.Stages = map[string]workflow.Stage{"draft": child.Stages["draft"], "check": {Primitive: workflow.Gate, Name: "child success", Expr: &ok, Failure: "halt"}, "halt": {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "child failed"}}
	child.Edges = []workflow.Edge{{From: "draft", To: "check"}, {From: "check", To: "Done"}, {From: "check", To: "halt"}}
	parent := childWorkflow()
	parent.ID = "join-parent"
	parent.Start = "fan"
	parent.Artifacts["Consult"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	parent.Artifacts["Joined"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"array","items":{"type":"object","required":["index","status"]}}`)}
	parent.Stages = map[string]workflow.Stage{
		"fan":     {Primitive: workflow.FanOut, Parallel: true, Join: "join", Branches: []workflow.FanBranch{{ID: "child", Start: "consult"}, {ID: "sibling", Start: "sibling"}}},
		"consult": {Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: child.ID, Revision: 1}, Output: "Consult", Failure: "join", OutputMap: []workflow.ArtifactMap{{Name: "note", Stage: "draft", Type: "Note"}}},
		"sibling": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"},
		"after":   {Primitive: workflow.Branch, Cases: []workflow.Case{{When: workflow.Expr{Op: "literal", Value: true}, Next: "join"}}, Default: "join"},
		"join":    {Primitive: workflow.FanIn, Rule: workflow.JoinAll, Output: "Joined", Failure: "stop"},
		"stop":    {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "join failed"},
	}
	parent.Edges = []workflow.Edge{{From: "fan", To: "consult"}, {From: "fan", To: "sibling"}, {From: "consult", To: "join"}, {From: "consult", To: "after"}, {From: "after", To: "join"}, {From: "sibling", To: "join"}, {From: "join", To: "Done"}, {From: "join", To: "stop"}}
	output := json.RawMessage(`{"ok":true,"text":"child note"}`)
	if fail {
		output = json.RawMessage(`{"ok":false,"text":"child rejected"}`)
	}
	return currentWorkflowFixture(parent), workflow.Scenario{Seed: 17, Workflows: map[string]workflow.Document{child.ID: child}, Rules: []workflow.ScenarioRule{{Stage: "draft", Output: output}, {Stage: "sibling", Output: json.RawMessage(`{"text":"sibling"}`)}}}
}
func assertSubworkflowJoin(t *testing.T, evidence []workflow.Evidence, failed bool) {
	t.Helper()
	var join *workflow.Evidence
	for i := range evidence {
		if evidence[i].Type == "Joined" {
			if join != nil {
				t.Fatal("duplicate join artifact")
			}
			join = &evidence[i]
		}
	}
	if join == nil {
		t.Fatal("join artifact absent")
	}
	var items []map[string]any
	if err := json.Unmarshal(join.Value, &items); err != nil || len(items) != 2 {
		t.Fatal("join list", string(join.Value), err)
	}
	child := items[0]
	if child["branch"] != "child" || child["index"] != float64(0) {
		t.Fatal("branch order changed", items)
	}
	if failed {
		if child["status"] != "failed" || child["error"] == nil || child["error"] == "" {
			t.Fatal("failed child became passed at its failure join", child)
		}
		return
	}
	if child["status"] != "passed" || child["artifact_id"] == nil || child["output"] == nil {
		t.Fatal("passing subworkflow omitted selected output", child)
	}
	output := child["output"].(map[string]any)
	outputs, ok := output["outputs"].(map[string]any)
	if !ok {
		t.Fatal(output)
	}
	note, ok := outputs["note"].(map[string]any)
	if !ok || note["text"] != "child note" {
		t.Fatal("mapped child artifact absent", output)
	}
}
func TestSubworkflowJoinFailureAndImplicitOutputOffline(t *testing.T) {
	for _, failed := range []bool{false, true} {
		doc, plan := subworkflowJoinFixture(failed)
		sim, err := engine.SimulateScenario(doc, "parent", plan, workflow.SimulationOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want := domain.Done
		if failed {
			want = domain.Escalated
		}
		if sim.State != want {
			t.Fatal(sim.State, sim.Reason)
		}
		assertSubworkflowJoin(t, sim.Evidence, failed)
	}
}
func TestSubworkflowJoinFailureAndImplicitOutputNative(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pass", true: "fail"}[failed], func(t *testing.T) {
			cfg, ctx := fixture(t)
			doc, plan := subworkflowJoinFixture(failed)
			cfg.Executor = engine.NewScenarioScripted(plan)
			d, err := Start(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer stop(t, d)
			authorize(t, d)
			governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
			if _, err = call(t, d, "Admit", AdmitParams{ID: "join-root", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "join-root"); err != nil {
				t.Fatal(err)
			}
			want := "Done"
			if failed {
				want = "Escalated"
			}
			awaitState(t, d, "join-root", want)
			row, err := d.store.GetJob(ctx, "join-root")
			if err != nil {
				t.Fatal(err)
			}
			var e envelope
			if err = decodeEnvelope(row.Payload, &e); err != nil {
				t.Fatal(err)
			}
			assertSubworkflowJoin(t, e.Job.Evidence, failed)
		})
	}
}

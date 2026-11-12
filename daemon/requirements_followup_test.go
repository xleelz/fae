package daemon

import (
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
)

func TestConfiguredRecursionCeilingAndDetachedBindingsNative(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.RecursionCeiling = 12
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"make": {{Output: json.RawMessage(`{"text":"recorded"}`)}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	doc := typedTreeWorkflow("deeper", false)
	doc.Recursion.MaxDepth = 9
	raw, _ := json.Marshal(doc)
	if _, err := workflow.ParseWrite(raw); err == nil {
		t.Fatal("default ceiling accepted depth9")
	}
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "root", WorkflowID: doc.ID, Tree: "tree", Inputs: map[string]any{"scope": "src"}, AuthorizingMessage: "message"}, "root"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "root", "Done")
	if _, err = call(t, d, "Admit", AdmitParams{ID: "foreign", WorkflowID: doc.ID, ParentTask: "root", MaxSessions: 4, AuthorizingMessage: "message"}, "bad"); err == nil || !strings.Contains(err.Error(), "detached id") || !strings.Contains(err.Error(), "max_sessions") || !strings.Contains(err.Error(), "inputs.scope") {
		t.Fatal("aggregate binding refusal missing", err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{WorkflowID: doc.ID, ParentTask: "root", FromParent: map[string]string{"scope": "scope"}, AuthorizingMessage: "message"}, "detached"); err != nil {
		t.Fatal(err)
	}
	child := awaitState(t, d, "root.detached.1", "Done")
	var e envelope
	if decodeEnvelope(child.Payload, &e) != nil || e.Job.Tree != "tree" {
		t.Fatal("child did not retain parent tree")
	}
	events, err := d.store.JobEvents(ctx, "root.detached.1", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("missing child events")
	}
	for _, event := range events {
		if event.ParentTask != "root" || event.TreeRoot != "root" {
			t.Fatal("metadata missing at write", event)
		}
	}
}

func TestFanVisitChildBudgetCountsEveryBranchNativeAndOffline(t *testing.T) {
	child := typedTreeWorkflow("visit-budget-child", false)
	child.Start = "make"
	makeStage := child.Stages["make"]
	makeStage.Inputs = nil
	child.Stages = map[string]workflow.Stage{"make": makeStage}
	child.Edges = []workflow.Edge{{From: "make", To: "Done"}}
	child.Signature.Inputs = nil
	parent := child
	parent.ID = "visit-budget-root"
	parent.Start = "discover"
	parent.Signature = nil
	parent.Artifacts = map[string]workflow.ArtifactType{"Contract": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}, "List": {Version: 1, Schema: json.RawMessage(`{"type":"object","properties":{"items":{"type":"array"}},"required":["items"]}`)}, "Joined": {Version: 1, Schema: json.RawMessage(`{"type":"array"}`)}}
	role := child.Roles["author"]
	role.Outputs = []string{"List", "Contract"}
	parent.Roles = map[string]workflow.Role{"author": role}
	pass := workflow.Expr{Op: "exists", Stage: "consult"}
	parent.Stages = map[string]workflow.Stage{
		"discover": {Primitive: workflow.AgentTurn, Role: "author", Output: "List"},
		"fan":      {Primitive: workflow.FanOut, Join: "join", Parallel: true, BranchesFrom: &workflow.BranchesFromSpec{Stage: "discover", Field: "items", Min: 1, Max: 3}, BranchTemplate: &workflow.FanBranch{ID: "template", Start: "consult", Pass: &pass}},
		"consult":  {Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: child.ID, Revision: 1}, Output: "Contract", Failure: "recover", OutputMap: []workflow.ArtifactMap{{Name: "contract", Output: "contract", Type: "Contract"}}},
		"recover":  {Primitive: workflow.AgentTurn, Role: "author", Output: "Contract", Inputs: []workflow.Input{{Name: "failure", Stage: "failure:consult", Type: "failure", Version: 1}}},
		"join":     {Primitive: workflow.FanIn, Output: "Joined", Rule: workflow.JoinSettle, Failure: "stop"}, "stop": {Primitive: workflow.Escalate, Kind: "HumanDecisionRequired", Reason: "failed"},
	}
	parent.Edges = []workflow.Edge{{From: "discover", To: "fan"}, {From: "fan", To: "consult"}, {From: "consult", To: "join"}, {From: "consult", To: "recover"}, {From: "recover", To: "join"}, {From: "join", To: "Done"}, {From: "join", To: "stop"}}
	plan := workflow.Scenario{Workflows: map[string]workflow.Document{child.ID: child}, Rules: []workflow.ScenarioRule{{Stage: "discover", Output: json.RawMessage(`{"items":[{"id":"a"},{"id":"b"},{"id":"c"}]}`)}}}
	report, err := engine.SimulateScenario(parent, "budget-root", plan, workflow.SimulationOptions{})
	if err != nil || report.State != "Done" || len(report.Tree) != 3 {
		t.Fatal("offline aggregate budget", err, report)
	}
	cfg, ctx := fixture(t)
	cfg.MaxInFlight = 2
	cfg.Executor = engine.NewScenarioScripted(plan)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: parent.ID, Document: parent, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "budget-root", WorkflowID: parent.ID, Tree: "tree", AuthorizingMessage: "message"}, "budget-root"); err != nil {
		t.Fatal(err)
	}
	row := awaitState(t, d, "budget-root", "Done")
	var root envelope
	if err := decodeEnvelope(row.Payload, &root); err != nil {
		t.Fatal(err)
	}
	rows, err := d.store.TreeJobs(ctx, "budget-root")
	if err != nil || len(rows) != 3 {
		t.Fatal("native child budget", len(rows), err)
	}
	found := false
	for _, a := range root.Job.Evidence {
		found = found || a.Type == "failure" && a.Fields["reason"] == "recursion_budget"
	}
	if !found {
		t.Fatal("missing visit-budget failure route evidence")
	}
	if err := engine.ReplayGates(root.Job); err != nil {
		t.Fatal("branch recovery proof does not replay", err)
	}
}

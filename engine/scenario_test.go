package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"reflect"
	"testing"
)

func scenarioLeafDoc() workflow.Document {
	d := dataWorkflow()
	d.Start = "make"
	d.Stages = map[string]workflow.Stage{"make": d.Stages["make"]}
	d.Edges = []workflow.Edge{{From: "make", To: "Done"}}
	d.Signature.Inputs = nil
	return d
}
func TestScenarioExecutesRecursiveChildrenAndReportsBudgetRefusal(t *testing.T) {
	d := scenarioLeafDoc()
	d.ID = "recursive"
	d.Start = "child"
	d.Artifacts["Note"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	d.Stages["child"] = workflow.Stage{Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: "self"}, Output: "Note", OutputMap: []workflow.ArtifactMap{{Name: "note", Output: "note", Type: "Note"}}, Failure: "make"}
	d.Edges = append(d.Edges, workflow.Edge{From: "child", To: "Done"}, workflow.Edge{From: "child", To: "make"})
	d.Recursion = &workflow.Recursion{MaxDepth: 3, MaxDescendants: 5, MaxChildrenPerVisit: 2}
	report, err := engine.SimulateScenario(d, "root", workflow.Scenario{Seed: 42, Rules: []workflow.ScenarioRule{{Stage: "make", Output: json.RawMessage(`{"text":"leaf"}`)}}}, workflow.SimulationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.State != domain.Done || len(report.Tree) != 3 || len(report.Refusals) != 1 {
		t.Fatalf("not real bounded child tree: %+v", report)
	}
	for _, node := range report.Tree {
		if node.State != domain.Done {
			t.Fatalf("unfinished child: %+v", node)
		}
	}
}
func TestScenarioDetachedChildrenAndSeededGenerationShareScriptedAdapter(t *testing.T) {
	d := scenarioLeafDoc()
	child := scenarioLeafDoc()
	child.ID = "detached"
	scenario := workflow.Scenario{Seed: 7, Workflows: map[string]workflow.Document{"detached": child}, Rules: []workflow.ScenarioRule{{Stage: "make", TaskGlob: "root", Output: json.RawMessage(`{"text":"root literal"}`), Detached: []workflow.ScenarioAdmission{{Workflow: "detached"}}}}}
	report, err := engine.SimulateScenario(d, "root", scenario, workflow.SimulationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.State != domain.Done || len(report.Tree) != 2 || !report.Tree[1].Detached || len(report.Tree[1].Generated) != 1 {
		t.Fatalf("detached generation missing: %+v", report)
	}
	again, err := engine.SimulateScenario(d, "root", scenario, workflow.SimulationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Tree[1].Evidence, again.Tree[1].Evidence) {
		t.Fatal("same seed produced different child evidence")
	}
	j, err := engine.New("root.detached.1", 1, child, "simulated-tree")
	if err != nil {
		t.Fatal(err)
	}
	j, err = engine.BindTaskInputs(j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	j, dispatch, _, err := engine.Prepare(j)
	if dispatch == nil && err == nil {
		j, dispatch, _, err = engine.Prepare(j)
	}
	if err != nil || dispatch == nil {
		t.Fatal(err, j.State, j.Reason)
	}
	r, err := engine.NewScenarioScripted(scenario).Execute(context.Background(), *dispatch)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	_ = json.Unmarshal(r.Output, &fields)
	if !reflect.DeepEqual(fields, report.Tree[1].Evidence[0].Fields) {
		t.Fatalf("offline/scripted diverged: %s vs %+v", r.Output, report.Tree[1].Evidence)
	}
}

func TestScenarioPerItemDecisionAndDynamicChildBranches(t *testing.T) {
	doc := itemDecisionWorkflow()
	items := []any{}
	choices := []workflow.ItemDecision{}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("part-%d", i)
		items = append(items, map[string]any{"id": id})
		choices = append(choices, workflow.ItemDecision{ID: id, Option: "accept", Rationale: "checked"})
	}
	output, _ := json.Marshal(map[string]any{"items": items})
	report, err := engine.SimulateScenario(doc, "items", workflow.Scenario{Rules: []workflow.ScenarioRule{{Stage: "inventory", Output: output}, {Stage: "choose", Items: choices}}}, workflow.SimulationOptions{})
	if err != nil || report.State != domain.Done {
		t.Fatalf("peritem scenario: %v %+v", err, report)
	}
	child := scenarioLeafDoc()
	child.ID = "branch-child"
	parent := scenarioLeafDoc()
	parent.ID = "dynamic-parent"
	parent.Start = "discover"
	parent.Signature = nil
	parent.Artifacts["Note"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	parent.Artifacts["List"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","required":["items"],"properties":{"items":{"type":"array"}}}`)}
	role := parent.Roles["author"]
	role.Outputs = append(role.Outputs, "List")
	parent.Roles["author"] = role
	pass := workflow.Expr{Op: "exists", Stage: "consult"}
	parent.Artifacts["Joined"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"array","items":{"type":"object"}}`)}
	parent.Stages = map[string]workflow.Stage{"discover": {Primitive: workflow.AgentTurn, Role: "author", Output: "List"}, "fan": {Primitive: workflow.FanOut, Join: "join", Parallel: true, BranchesFrom: &workflow.BranchesFromSpec{Stage: "discover", Field: "items", Min: 1, Max: 3}, BranchTemplate: &workflow.FanBranch{ID: "template", Start: "consult", Pass: &pass}}, "consult": {Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: child.ID, Revision: 1}, Output: "Note", OutputMap: []workflow.ArtifactMap{{Name: "note", Output: "note", Type: "Note"}}}, "join": {Primitive: workflow.FanIn, Output: "Joined", Rule: workflow.JoinAll, Failure: "stop"}, "stop": {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "join failed"}}
	parent.Edges = []workflow.Edge{{From: "discover", To: "fan"}, {From: "fan", To: "consult"}, {From: "consult", To: "join"}, {From: "join", To: "Done"}, {From: "join", To: "stop"}}
	report, err = engine.SimulateScenario(parent, "dynamic", workflow.Scenario{Seed: 8, Workflows: map[string]workflow.Document{child.ID: child}, Rules: []workflow.ScenarioRule{{Stage: "discover", Output: json.RawMessage(`{"items":[{"id":"one"},{"id":"two"}]}`)}}}, workflow.SimulationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.State != domain.Done || len(report.Tree) != 3 {
		t.Fatalf("dynamic children did not execute: %+v", report)
	}
	for _, node := range report.Tree[1:] {
		if node.State != domain.Done || len(node.Generated) != 1 {
			t.Fatalf("branch child missing real generated output: %+v", node)
		}
	}
}

func TestScenarioRuleFirstMatchHonorsRoleTaskGlobAndBranchIndex(t *testing.T) {
	d := engine.Dispatch{ID: "d", TaskID: "root.detached.1", Stage: "make", Visit: 1, Tree: "tree", BranchID: "outer/branch-2", Config: workflow.Stage{Primitive: workflow.AgentTurn}, Sessions: []engine.Session{{ID: "session", Role: "author"}}, OutputSchema: json.RawMessage(`{"type":"object"}`)}
	two := 2
	one := 1
	plan := workflow.Scenario{Rules: []workflow.ScenarioRule{{Stage: "make", Role: "other", Output: json.RawMessage(`{"wrong":"role"}`)}, {Stage: "make", TaskGlob: "different*", Output: json.RawMessage(`{"wrong":"task"}`)}, {Stage: "make", BranchIndex: &one, Output: json.RawMessage(`{"wrong":"branch"}`)}, {Stage: "make", Role: "author", Task: "root.*", BranchIndex: &two, Output: json.RawMessage(`{"chosen":true}`)}, {Stage: "make", Output: json.RawMessage(`{"wrong":"later"}`)}}}
	r, err := engine.NewScenarioScripted(plan).Execute(context.Background(), d)
	if err != nil || string(r.Output) != `{"chosen":true}` {
		t.Fatalf("wrong firstmatch: %s %v", r.Output, err)
	}
}

func TestScenarioChildChangeRequestIsAddressedToAncestor(t *testing.T) {
	child := scenarioLeafDoc()
	child.ID = "request-child"
	parent := scenarioLeafDoc()
	parent.ID = "request-parent"
	parent.Start = "consult"
	parent.Artifacts["Note"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	parent.Stages = map[string]workflow.Stage{"consult": {Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: child.ID, Revision: 1}, Output: "Note", OutputMap: []workflow.ArtifactMap{{Name: "note", Output: "note", Type: "Note"}}}}
	parent.Edges = []workflow.Edge{{From: "consult", To: "Done"}}
	plan := workflow.Scenario{Workflows: map[string]workflow.Document{child.ID: child}, Rules: []workflow.ScenarioRule{{Stage: "make", Task: "root.*", Output: json.RawMessage(`{"text":"child result"}`), ChangeRequest: &workflow.ScenarioChangeRequest{Target: "parent", Artifact: "Note", Value: map[string]any{"text": "revise assumption"}}}}}
	report, err := engine.SimulateScenario(parent, "root", plan, workflow.SimulationOptions{})
	if err != nil || report.State != domain.Done || len(report.Tree) != 2 || len(report.Tree[1].ChangeRequests) != 1 {
		t.Fatalf("missing child request: %v %+v", err, report)
	}
	var request engine.AncestorChangeRequest
	_ = json.Unmarshal(report.Tree[1].ChangeRequests[0], &request)
	if request.Target != "root" || request.State != "pending" {
		t.Fatalf("request autoapplied or misaddressed: %+v", request)
	}
}

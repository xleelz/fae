package engine_test

import (
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
)

func TestLoopedFanStartsFreshChildrenAndChangeRequests(t *testing.T) {
	leaf := scenarioLeafDoc()
	leaf.ID = "identity-leaf"
	worker := branchRecoveryDoc(2)
	worker.ID = "identity-worker"
	worker.Start = "repeat"
	worker.Artifacts["Note"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	worker.Artifacts["Published"] = leaf.Artifacts["Note"]
	fan := worker.Stages["fan"]
	fan.BranchTemplate.Output = "Published"
	worker.Stages["fan"] = fan
	worker.Stages["repeat"] = workflow.Stage{Primitive: workflow.Loop, MaxIterations: 3}
	worker.Stages["work"] = workflow.Stage{Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: leaf.ID, Revision: 1}, Output: "Note", OutputMap: []workflow.ArtifactMap{{Name: "note", Output: "note", As: "Published"}}}
	worker.Stages["request"] = workflow.Stage{Primitive: workflow.ChangeRequest, Target: "parent", Artifact: "Note", Value: &workflow.Projection{Literal: json.RawMessage(`{"text":"review"}`)}}
	filter := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "item", Path: []string{"output", "text"}}, {Op: "literal", Value: "second"}}}
	done := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "count", Stage: "join", Path: []string{"items"}, Args: []workflow.Expr{filter}}, {Op: "literal", Value: 2}}}
	worker.Stages["again"] = workflow.Stage{Primitive: workflow.Branch, Cases: []workflow.Case{{When: done, Next: "Done"}}, Default: "repeat"}
	worker.Edges = []workflow.Edge{{From: "repeat", To: "fan"}, {From: "fan", To: "work"}, {From: "work", To: "request"}, {From: "request", To: "join"}, {From: "join", To: "again"}, {From: "join", To: "stop"}, {From: "again", To: "Done"}, {From: "again", To: "repeat"}}
	harness := scenarioLeafDoc()
	harness.ID = "identity-harness"
	harness.Signature = nil
	harness.Artifacts["Note"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	harness.Start = "consult"
	harness.Stages = map[string]workflow.Stage{"consult": {Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: worker.ID, Revision: 1}, Output: "Note", OutputMap: []workflow.ArtifactMap{{Name: "joined", Stage: "join", Type: "Joined"}}}}
	harness.Edges = []workflow.Edge{{From: "consult", To: "Done"}}
	report, err := engine.SimulateScenario(harness, "root", workflow.Scenario{Workflows: map[string]workflow.Document{worker.ID: worker, leaf.ID: leaf}, Rules: []workflow.ScenarioRule{{Stage: "make", TaskGlob: "*.fan.2", Output: json.RawMessage(`{"text":"second"}`)}, {Stage: "make", Output: json.RawMessage(`{"text":"first"}`)}}}, workflow.SimulationOptions{})
	if err != nil || report.State != domain.Done {
		t.Fatalf("loop reused completed child: %s %s %v", report.State, report.Reason, err)
	}
	children := map[string]bool{}
	requests := map[string]bool{}
	for _, node := range report.Tree {
		if node.Workflow == leaf.ID {
			children[node.Task] = true
		}
		if node.Workflow == worker.ID {
			for _, raw := range node.ChangeRequests {
				var r engine.AncestorChangeRequest
				_ = json.Unmarshal(raw, &r)
				requests[r.ID] = true
			}
		}
	}
	if len(children) != 4 || len(requests) != 4 {
		t.Fatal("later fan reused child/request", children, requests)
	}
	later := 0
	for id := range children {
		if strings.HasSuffix(id, ".fan.2") {
			later++
		}
	}
	if later != 2 {
		t.Fatal(children)
	}
}

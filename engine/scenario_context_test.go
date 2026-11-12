package engine_test

import (
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"path/filepath"
	"testing"
)

func TestScenarioBindsTaskDirectoriesForHostCommands(t *testing.T) {
	doc := scenarioLeafDoc()
	doc.Signature = nil
	doc.Staging = true
	doc.Produces = "artifacts"
	doc.Start = "check"
	argv := []string{"checker", "{{output_dir}}/x", "{{staging_dir}}/accepted"}
	doc.TargetRequirements = &workflow.TargetRequirements{Commands: [][]string{argv}}
	doc.Stages = map[string]workflow.Stage{"check": {Primitive: workflow.HostCommand, Weight: 1, TimeoutSeconds: 10, Command: argv, Output: "Note", Check: &workflow.CheckSpec{Format: "json", CountField: "checked"}}}
	doc.Edges = []workflow.Edge{{From: "check", To: "Done"}}
	report, err := engine.SimulateScenario(doc, "root", workflow.Scenario{ScratchRoot: filepath.Join(t.TempDir(), "rehearsal"), Rules: []workflow.ScenarioRule{{Stage: "check", Output: json.RawMessage(`{"text":"checked","checked":1,"exit_code":0,"timed_out":false,"truncated":false}`)}}}, workflow.SimulationOptions{})
	if err != nil || report.State != domain.Done {
		t.Fatal(err, report.State, report.Reason)
	}
}
func TestScenarioForwardsIncomingRequestBeforeParentOutputExists(t *testing.T) {
	leaf := scenarioLeafDoc()
	leaf.ID = "leaf"
	leaf.Stages["request"] = workflow.Stage{Primitive: workflow.ChangeRequest, Target: "parent", Artifact: "note", Value: &workflow.Projection{Stage: "make", Field: "text"}}
	leaf.Edges = []workflow.Edge{{From: "make", To: "request"}, {From: "request", To: "Done"}}
	wrapper := scenarioLeafDoc()
	wrapper.ID = "wrapper"
	wrapper.Start = "consult"
	wrapper.Artifacts["Note"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	wrapper.Stages = map[string]workflow.Stage{"consult": {Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: "leaf", Revision: 1}, Output: "Note", OutputMap: []workflow.ArtifactMap{{Name: "note", Output: "note", Type: "Note"}}}, "forward": {Primitive: workflow.ChangeRequest, Target: "parent", Artifact: "Note", Value: &workflow.Projection{Source: "change_requests"}}}
	wrapper.Edges = []workflow.Edge{{From: "consult", To: "forward"}, {From: "forward", To: "Done"}}
	root := scenarioLeafDoc()
	root.ID = "top"
	root.Start = "consult"
	root.Artifacts["Note"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	root.Stages = map[string]workflow.Stage{"consult": {Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: "wrapper", Revision: 1}, Output: "Note", OutputMap: []workflow.ArtifactMap{{Name: "note", Output: "note", Type: "Note"}}}}
	root.Edges = []workflow.Edge{{From: "consult", To: "Done"}}
	report, err := engine.SimulateScenario(root, "root", workflow.Scenario{Workflows: map[string]workflow.Document{"leaf": leaf, "wrapper": wrapper}, Rules: []workflow.ScenarioRule{{Stage: "make", Output: json.RawMessage(`{"text":"revise ontology"}`)}}}, workflow.SimulationOptions{})
	if err != nil || report.State != domain.Done || len(report.Tree) != 3 || len(report.Tree[0].IncomingRequests) != 1 {
		t.Fatalf("forward: %v %s %s %+v", err, report.State, report.Reason, report.Tree)
	}
	var forwarded engine.AncestorChangeRequest
	if json.Unmarshal(report.Tree[0].IncomingRequests[0], &forwarded) != nil {
		t.Fatal("invalid forwarded request")
	}
	values, ok := forwarded.Value.([]any)
	if !ok || len(values) != 1 || values[0].(map[string]any)["value"] != "revise ontology" {
		t.Fatal("incoming request value lost", forwarded.Value)
	}
}

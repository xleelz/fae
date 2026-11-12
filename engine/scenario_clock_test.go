package engine_test

import (
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
	"time"
)

func TestScenarioVirtualDelaysAndRoleTreeCaps(t *testing.T) {
	for _, test := range []struct {
		name                   string
		roleCap, treeCap, peak int
		duration               float64
	}{
		{"parallel", 4, -1, 4, 10000},
		{"tree cap", 4, 2, 2, 20000},
		{"role cap", 1, -1, 1, 40000},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := nestedDoc()
			role := doc.Roles["author"]
			role.Cap = test.roleCap
			role.CapScope = "tree"
			doc.Roles["author"] = role
			doc.SlotScope = "tree"
			doc.MaxSessions = test.treeCap
			rules := []workflow.ScenarioRule{}
			for _, stage := range []string{"leaf", "innerleaf", "middleleaf", "sibling"} {
				rules = append(rules, workflow.ScenarioRule{Stage: stage, Delay: 10 * time.Second, Output: json.RawMessage(`{"text":"result"}`)})
			}
			began := time.Now()
			report, err := engine.SimulateScenario(doc, "root", workflow.Scenario{Rules: rules}, workflow.SimulationOptions{})
			if err != nil || report.State != domain.Done || report.DurationMS != test.duration || report.PeakSessions != test.peak || len(report.Timeline) != 4 {
				t.Fatalf("simulation: %v state=%s duration=%v peak=%d timeline=%+v", err, report.State, report.DurationMS, report.PeakSessions, report.Timeline)
			}
			if time.Since(began) > 2*time.Second {
				t.Fatal("virtual simulation slept for scenario delay")
			}
		})
	}
}
func TestScenarioUnmatchedPatternOutputRunsToDone(t *testing.T) {
	doc := scenarioLeafDoc()
	artifact := doc.Artifacts["Note"]
	artifact.Schema = json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string","pattern":"^[A-Z]{3}$"}}}`)
	doc.Artifacts["Note"] = artifact
	report, err := engine.SimulateScenario(doc, "root", workflow.Scenario{Seed: 7}, workflow.SimulationOptions{})
	if err != nil || report.State != domain.Done || len(report.Tree) != 1 || len(report.Tree[0].Generated) != 1 {
		t.Fatalf("unmatched schema generation: %v %+v", err, report)
	}
}
func TestScenarioParallelChildrenShareVirtualTime(t *testing.T) {
	child := scenarioLeafDoc()
	child.ID = "child"
	parent := nestedDoc()
	parent.Artifacts["Note"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	parent.SlotScope = "tree"
	parent.MaxSessions = 4
	for _, stage := range []string{"leaf", "innerleaf", "middleleaf", "sibling"} {
		parent.Stages[stage] = workflow.Stage{Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: "child", Revision: 1}, Output: "Note", OutputMap: []workflow.ArtifactMap{{Name: "note", Output: "note", Type: "Note"}}}
	}
	report, err := engine.SimulateScenario(parent, "root", workflow.Scenario{Workflows: map[string]workflow.Document{"child": child}, Rules: []workflow.ScenarioRule{{Stage: "make", Delay: 10 * time.Second, Output: json.RawMessage(`{"text":"child"}`)}}}, workflow.SimulationOptions{})
	// The child role's default workflow-scoped cap1 serializes all four tasks.
	if err != nil || report.State != domain.Done || report.DurationMS != 40000 || len(report.Tree) != 5 {
		t.Fatalf("child scheduling: %v state=%s reason=%s duration=%v tree=%+v", err, report.State, report.Reason, report.DurationMS, report.Tree)
	}
	role := child.Roles["author"]
	role.Cap = 4
	child.Roles["author"] = role
	report, err = engine.SimulateScenario(parent, "root", workflow.Scenario{Workflows: map[string]workflow.Document{"child": child}, Rules: []workflow.ScenarioRule{{Stage: "make", Delay: 10 * time.Second, Output: json.RawMessage(`{"text":"child"}`)}}}, workflow.SimulationOptions{})
	if err != nil || report.State != domain.Done || report.DurationMS != 10000 || report.PeakSessions != 4 {
		t.Fatalf("parallel child scheduling: %v state=%s duration=%v peak=%d", err, report.State, report.DurationMS, report.PeakSessions)
	}
}

package engine

import (
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
	"testing"
)

func TestLaterFanVisitsQualifyChildRequestAndDecisionIdentity(t *testing.T) {
	context := &BranchContext{Fan: "inner", Path: "left/right", IndexPath: []int{0, 1}, VisitGroups: map[string]int{"/outer": 1, "left/inner": 1}}
	j := Job{TaskID: "root", Revision: 1, WorkflowHash: "hash", BranchContext: context}
	if got := childVisitID(j, "consult", 1, -1); got != "root.consult.1.0.1" {
		t.Fatal(got)
	}
	decisionFirst := decisionID(j, "choose", 1)
	context.VisitGroups["left/inner"] = 2
	if got := childVisitID(j, "consult", 1, -1); got != "root.consult.1.0.1.fan.1.2" {
		t.Fatal(got)
	}
	if decisionFirst == decisionID(j, "choose", 1) {
		t.Fatal("decision reused across fan visits")
	}
	context.VisitGroups = map[string]int{"left/inner": 1, "/outer": 2}
	if got := childVisitID(j, "consult", 1, -1); got != "root.consult.1.0.1.fan.2.1" {
		t.Fatal(got)
	}
	j.State = "request"
	j.Counts = map[string]int{"request": 1}
	j.Parent = &ParentRef{Task: "ancestor"}
	j.Ancestors = []string{"ancestor"}
	j.Disposition = domain.Disposition{Mode: domain.Local}
	s := workflow.Stage{Primitive: workflow.ChangeRequest, Target: "parent", Artifact: "Note", Value: &workflow.Projection{Literal: json.RawMessage(`{"text":"same"}`)}}
	j.Document = workflow.Document{Stages: map[string]workflow.Stage{"request": s}, Edges: []workflow.Edge{{From: "request", To: "Done"}}}
	events := []Event{}
	if err := changeRequestStep(&j, s, &events); err != nil {
		t.Fatal(err)
	}
	first := j.AncestorRequests[0].ID
	j.State = "request"
	context.VisitGroups["/outer"] = 3
	if err := changeRequestStep(&j, s, &events); err != nil {
		t.Fatal(err)
	}
	if len(j.AncestorRequests) != 2 || first == j.AncestorRequests[1].ID {
		t.Fatal("request reused", j.AncestorRequests)
	}
}

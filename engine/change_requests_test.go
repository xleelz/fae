package engine_test

import (
	"errors"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
)

func TestIncomingChangeRequestsRequireExplicitProjection(t *testing.T) {
	j := engine.Job{State: "forward", Document: workflow.Document{Stages: map[string]workflow.Stage{"forward": {Primitive: workflow.ChangeRequest}}}, IncomingRequests: []engine.AncestorChangeRequest{{ID: "request", Source: "child", Target: "parent", Artifact: "Note", Value: map[string]any{"text": "requested"}, State: "pending"}}}
	value, ids, err := engine.ResolveProjection(j, workflow.Projection{Source: "change_requests"})
	if err != nil || len(ids) != 1 || ids[0] != "change-request:request" {
		t.Fatal(value, ids, err)
	}
	j.State = "agent"
	if _, _, err := engine.ResolveProjection(j, workflow.Projection{Source: "change_requests"}); err == nil {
		t.Fatal("incoming requests exposed outside forwarding stage")
	}
	rows, ok := value.([]any)
	if !ok || len(rows) != 1 || rows[0].(map[string]any)["source"] != "child" {
		t.Fatal(value)
	}
}
func TestChangeRequestTargetValidationUsesFailureEdge(t *testing.T) {
	doc := defaults.Workflow()
	oldStart := doc.Start
	doc.Start = "request"
	doc.Stages["request"] = workflow.Stage{Primitive: workflow.ChangeRequest, Target: "parent", Artifact: "Missing", Value: &workflow.Projection{Source: "tree"}, Failure: "handler"}
	doc.Stages["handler"] = workflow.Stage{Primitive: workflow.Escalate, Kind: "HumanDecisionRequired", Reason: "request failed"}
	doc.Edges = append(doc.Edges, workflow.Edge{From: "request", To: oldStart}, workflow.Edge{From: "request", To: "handler"})
	j, err := engine.New("child", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	for i := 0; i < 5; i++ {
		next, _, _, e := engine.PrepareWith(j, engine.Options{ValidateChangeRequest: func(engine.Job, workflow.Stage) error {
			called = true
			return errors.New("ancestor artifact Missing does not exist")
		}})
		if e != nil {
			t.Fatal(e)
		}
		j = next
		if called {
			break
		}
	}
	if !called || string(j.State) != "handler" || len(j.AncestorRequests) != 0 {
		t.Fatalf("state=%s called=%v req=%+v", j.State, called, j.AncestorRequests)
	}
	found := false
	for _, a := range j.Evidence {
		if a.Type == "failure" {
			found = true
		}
	}
	if !found {
		t.Fatal("failure route omitted failure evidence")
	}
}

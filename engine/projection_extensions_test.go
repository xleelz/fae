package engine_test

import (
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
)

func TestProjectionIndexThenLookupLiteralAndMissingDefault(t *testing.T) {
	doc := scenarioLeafDoc()
	doc.Signature = &workflow.Signature{Inputs: map[string]json.RawMessage{"parent": json.RawMessage(`{"type":"object"}`), "component": json.RawMessage(`{"type":"string"}`), "level": json.RawMessage(`{"type":"string"}`)}}
	j, err := engine.New("projection", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	j, err = engine.BindTaskInputs(j, map[string]any{"parent": map[string]any{"components": []any{map[string]any{"id": "wrong", "code_paths": []any{"wrong"}}, map[string]any{"id": "named", "code_paths": []any{"pkg/**"}}}}, "component": "named", "level": "service"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	filter := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "item", Path: []string{"id"}}, {Op: "field", Stage: "input:component", Path: []string{"value"}}}}
	p := workflow.Projection{Input: "parent", Field: "components", Where: &filter, Index: &zero, Then: "code_paths"}
	got, ids, err := engine.ResolveProjection(j, p)
	if err != nil || len(ids) < 2 {
		t.Fatal(got, ids, err)
	}
	raw, _ := json.Marshal(got)
	if string(raw) != `["pkg/**"]` {
		t.Fatal(string(raw))
	}
	lookup := workflow.Projection{Lookup: &workflow.ProjectionLookup{Table: map[string]json.RawMessage{"service": json.RawMessage(`"logic"`)}, Key: &workflow.Projection{Input: "level"}}}
	got, ids, err = engine.ResolveProjection(j, lookup)
	if err != nil || got != "logic" || len(ids) != 1 {
		t.Fatal(got, ids, err)
	}
	got, _, err = engine.ResolveProjection(j, workflow.Projection{Object: map[string]workflow.Projection{"empty": {Literal: json.RawMessage(`[]`)}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(got)
	if string(raw) != `{"empty":[]}` {
		t.Fatal(string(raw))
	}
	past := 99
	p.Index = &past
	if _, _, err = engine.ResolveProjection(j, p); err == nil {
		t.Fatal("missing index accepted")
	}
	p.Default = json.RawMessage(`[]`)
	got, _, err = engine.ResolveProjection(j, p)
	raw, _ = json.Marshal(got)
	if err != nil || string(raw) != `[]` {
		t.Fatal(string(raw), err)
	}
}
func TestExplicitChildProjectionDefaultIsRecordedWithReadEvidence(t *testing.T) {
	d := scenarioLeafDoc()
	d.ID = "default-parent"
	d.Signature = &workflow.Signature{Inputs: map[string]json.RawMessage{"items": json.RawMessage(`{"type":"array","maxItems":1,"default":[{}]}`)}}
	d.Stages = map[string]workflow.Stage{"call": {Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: "leaf", Revision: 1}, ItemsFrom: &workflow.Projection{Input: "items"}, InputMap: []workflow.ArtifactMap{{Name: "scenarios", Projection: &workflow.Projection{Source: "item", Field: "scenarios", Default: json.RawMessage(`[]`)}}}, OutputMap: []workflow.ArtifactMap{{Name: "note", Stage: "make", Type: "Note"}}, Output: "Note", Failure: "stop"}, "stop": {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "stop"}}
	d.Start = "call"
	d.Edges = []workflow.Edge{{From: "call", To: "Done"}, {From: "call", To: "stop"}}
	j, err := engine.New("root", 1, d, "tree")
	if err != nil {
		t.Fatal(err)
	}
	j, err = engine.BindTaskInputs(j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3 && j.Child == nil; i++ {
		j, _, _, err = engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
	}
	found := false
	for _, event := range j.History {
		if event.Kind == "projection-default" {
			found = true
			if len(event.EvidenceIDs) == 0 {
				t.Fatal("default lost collection evidence")
			}
		}
	}
	if !found {
		t.Fatal("default usage was not recorded", j.History)
	}
	inputs, _, err := engine.ChildMappedInputValues(j, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(inputs)
	if string(raw) != `{"scenarios":[]}` {
		t.Fatal(string(raw))
	}
}

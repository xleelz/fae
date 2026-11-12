package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCatalogChecksInputSchemasAndEveryDocumentInCycle(t *testing.T) {
	parent := Document{Signature: &Signature{Inputs: map[string]json.RawMessage{"n": json.RawMessage(`{"type":"string"}`)}}, Stages: map[string]Stage{"call": {Primitive: SubWorkflow, InputMap: []ArtifactMap{{Name: "number", Projection: &Projection{Input: "n"}}}}}}
	child := Document{Signature: &Signature{Inputs: map[string]json.RawMessage{"number": json.RawMessage(`{"type":"number"}`)}}}
	if issues := ValidateChildBindings(parent, "call", child); len(issues) == 0 {
		t.Fatal("incompatible child input schema accepted")
	}
	docs := map[string]Document{}
	names := []string{"a", "b", "c", "d"}
	for i, name := range names {
		d := Document{ID: name, Artifacts: map[string]ArtifactType{"Note": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}}, Stages: map[string]Stage{"call": {Primitive: SubWorkflow, Output: "Note", Workflow: &WorkflowRef{ID: names[(i+1)%len(names)], Revision: 1}, OutputMap: []ArtifactMap{{Name: "note", Stage: "call", Type: "Note"}}}}}
		if name != "c" {
			d.Recursion = &Recursion{MaxDepth: 8, MaxChildrenPerVisit: 2, MaxDescendants: 20}
		}
		docs[name] = d
	}
	issues := ValidateCatalog(docs["a"], func(ref WorkflowRef) (Document, error) {
		if d, ok := docs[ref.ID]; ok {
			return d, nil
		}
		return Document{}, fmt.Errorf("missing %s", ref.ID)
	})
	found := false
	for _, issue := range issues {
		found = found || strings.Contains(issue.Message, "every document: c")
	}
	if !found {
		t.Fatal(issues)
	}
}

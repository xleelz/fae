package workflow_test

import (
	"encoding/json"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
)

func TestWorkflowSignatureBindsDefaultsAndAllInputErrors(t *testing.T) {
	d := workflow.Document{Signature: &workflow.Signature{Inputs: map[string]json.RawMessage{"a": json.RawMessage(`{"type":"integer"}`), "b": json.RawMessage(`{"type":"string"}`), "c": json.RawMessage(`{"type":"boolean","default":true}`)}}}
	values, issues := workflow.BindInputs(d, map[string]any{"unknown": 1})
	text := ""
	for _, e := range issues {
		text += e.Error()
	}
	for _, name := range []string{"inputs.a", "inputs.b", "inputs.unknown"} {
		if !strings.Contains(text, name) {
			t.Errorf("missing issue %s: %s", name, text)
		}
	}
	if values["c"] != true {
		t.Fatal("missing default", values)
	}
	_, issues = workflow.BindInputs(d, map[string]any{"a": "wrong", "b": false})
	if len(issues) != 2 {
		t.Fatal("schema failures not aggregated", issues)
	}
}

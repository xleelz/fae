package main

import (
	"bytes"
	"encoding/json"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfflineWorkflowCatalogCompletesFootprintValidation(t *testing.T) {
	child := workflow.Document{SchemaVersion: 2, ID: "child", Start: "draft", TargetRequirements: &workflow.TargetRequirements{}, Limits: workflow.Limits{Attempts: 10, TestRuns: 1, ScopeWidens: 1}, Roles: map[string]workflow.Role{"author": {Instructions: "x", Briefs: map[string]string{"default": "x"}, Routes: []workflow.Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"Note"}}}, Artifacts: map[string]workflow.ArtifactType{"Note": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}}, Stages: map[string]workflow.Stage{"draft": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"}}, Edges: []workflow.Edge{{From: "draft", To: "Done"}}}
	parent := child
	parent.ID = "parent"
	parent.Start = "call"
	parent.Stages = map[string]workflow.Stage{"call": {Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: "child", Revision: 2}, Output: "Note", OutputMap: []workflow.ArtifactMap{{Name: "note", Stage: "draft", Type: "Note"}}}}
	parent.Edges = []workflow.Edge{{From: "call", To: "Done"}}
	dir := t.TempDir()
	file := filepath.Join(dir, "parent.json")
	catalog := filepath.Join(dir, "catalog.json")
	raw, _ := json.Marshal(parent)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(map[string]workflow.Document{"child@2": child})
	if err := os.WriteFile(catalog, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"workflow", "validate", file, "--catalog", catalog}, {"template", "workflow-validate", "--content", file, "--catalog", catalog}} {
		var out bytes.Buffer
		if err := run(args, &out); err != nil {
			t.Fatal(args, err, out.String())
		}
		var result struct {
			Footprint *workflow.Footprint `json:"footprint"`
		}
		_ = json.Unmarshal(out.Bytes(), &result)
		if result.Footprint == nil || result.Footprint.EffectiveSessions != 1 {
			t.Fatal(out.String())
		}
	}
	empty := filepath.Join(dir, "empty.json")
	scenario := filepath.Join(dir, "scenario.json")
	if err := os.WriteFile(empty, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scenario, []byte(`{"rules":[{"stage":"draft","output":{}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var rehearsed bytes.Buffer
	if err := run([]string{"workflow", "simulate", file, scenario, "root", "--catalog", empty, "--catalog", catalog, "--tree"}, &rehearsed); err != nil {
		t.Fatal(err, rehearsed.String())
	}
	if !strings.Contains(rehearsed.String(), "child@2 state=Done") || !strings.Contains(rehearsed.String(), "parent@1 state=Done") {
		t.Fatal(rehearsed.String())
	}
	var out bytes.Buffer
	if err := run([]string{"workflow", "validate", file}, &out); err == nil || !strings.Contains(out.String(), "child@2") {
		t.Fatal("unresolved child silently validated", err, out.String())
	}
}

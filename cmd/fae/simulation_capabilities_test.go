package main

import (
	"bytes"
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSimulationFailureExitAndQualifiedTreeRendering(t *testing.T) {
	var out bytes.Buffer
	r := workflow.Simulation{State: domain.Escalated, Reason: "binding: missing input", Tree: []workflow.SimulationTask{{Task: "root", Workflow: "parent", Revision: 2, State: domain.Escalated, Depth: 1, Reason: "binding", Branches: []workflow.SimulationBranch{{ID: "branch-0", FanStage: "areas", Visit: 2, Path: "branch-0", Outcome: "failed"}}, FailureRoutes: []workflow.FailureRouteReport{{Stage: "call", Handler: "repair", Reason: "binding"}}, Refusals: []workflow.RefusalReport{{Stage: "call", Reason: "binding", Detail: "missing input"}}}}}
	if err := simulationResult(r, true, &out); err == nil || !strings.Contains(err.Error(), "Escalated") {
		t.Fatal(err)
	}
	for _, text := range []string{"parent@2", "depth=1", "areas/branch-0 visit=2", "failure-route", "refusal"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("missing %s: %s", text, out.String())
		}
	}
}
func TestRepeatedCatalogFilesMergeAndRefuseConflicts(t *testing.T) {
	d := workflow.Document{SchemaVersion: 1, ID: "initial", Start: "stop", Roles: map[string]workflow.Role{"a": {Instructions: "x", Briefs: map[string]string{"default": "x"}, Routes: []workflow.Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"Note"}}}, Artifacts: map[string]workflow.ArtifactType{"Note": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}}, Limits: workflow.Limits{Attempts: 1, TestRuns: 1, ScopeWidens: 1}, Stages: map[string]workflow.Stage{"stop": {Primitive: workflow.Escalate}}}
	d.ID = "catalog-a"
	dir := t.TempDir()
	first := filepath.Join(dir, "one.json")
	second := filepath.Join(dir, "two.json")
	a, _ := json.Marshal(map[string]workflow.Document{d.ID: d})
	if err := os.WriteFile(first, a, 0600); err != nil {
		t.Fatal(err)
	}
	d.ID = "catalog-b"
	b, _ := json.Marshal(map[string]workflow.Document{d.ID: d})
	if err := os.WriteFile(second, b, 0600); err != nil {
		t.Fatal(err)
	}
	docs, err := loadWorkflowCatalogs([]string{first, second})
	if err != nil || len(docs) != 2 {
		t.Fatal(docs, err)
	}
	operands, err := offlineWorkflowOperands([]string{"simulate", "a.json", "b.json", "root", "--catalog", first, "--catalog", second, "--tree"}, common{})
	if err != nil || len(operands) != 9 {
		t.Fatal(operands, err)
	}
}

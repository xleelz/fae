package engine_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func bindingWorkflow() workflow.Document {
	object := json.RawMessage(`{"type":"object"}`)
	return workflow.Document{SchemaVersion: 1, ID: "bind", Start: "spec", Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 3},
		Roles:     map[string]workflow.Role{"writer": {Instructions: "write", Briefs: map[string]string{"default": "brief"}, Routes: []workflow.Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"Spec"}}},
		Artifacts: map[string]workflow.ArtifactType{"Spec": {Version: 1, Schema: json.RawMessage(`{"type":"object","properties":{"acceptance":{"type":"string"}}}`)}, "Run": {Version: 1, Schema: object}},
		Stages: map[string]workflow.Stage{
			"spec": {Primitive: workflow.AgentTurn, Role: "writer", Output: "Spec"},
			"run":  {Primitive: workflow.HostCommand, Output: "Run", Command: []string{"printf", "%s", "{{acceptance}}"}, Weight: 1, TimeoutSeconds: 5, Inputs: []workflow.Input{{Name: "spec", Type: "Spec", Stage: "spec", Version: 1}}, Bindings: map[string]workflow.ArgBinding{"acceptance": {Type: "text", Input: "spec", Field: "acceptance"}}},
		},
		Edges: []workflow.Edge{{From: "spec", To: "run"}, {From: "run", To: "Done"}},
	}
}

// FAE-WF-035: the durable dispatch carries the vector bound from artifacts,
// and a persisted vector that differs from its inputs is refused.
func TestDispatchBindsTemplatedCommand(t *testing.T) {
	hostile := "a; rm -rf / && $(id) | tee x"
	j, e := engine.New("task", 1, bindingWorkflow(), "tree")
	if e != nil {
		t.Fatal(e)
	}
	exec := engine.NewScripted(map[string][]engine.ScriptStep{"spec": {{Output: json.RawMessage(`{"acceptance":` + mustJSON(hostile) + `}`)}}, "run": {{Output: json.RawMessage(`{}`)}}})
	var run *engine.Dispatch
	for i := 0; i < 10 && run == nil; i++ {
		next, d, _, err := engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		j = next
		if d == nil {
			continue
		}
		if d.Stage == "run" {
			run = d
			break
		}
		r, err := exec.Execute(t.Context(), *d)
		if err != nil {
			t.Fatal(err)
		}
		if j, _, err = engine.Complete(j, d.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	if run == nil || !slices.Equal(run.Argv, []string{"printf", "%s", hostile}) || !slices.Equal(run.Vector(), run.Argv) {
		t.Fatalf("bound dispatch %+v", run)
	}
	if err := engine.Validate(roundTrip(t, j)); err != nil {
		t.Fatal(err)
	}
	tampered := roundTrip(t, j)
	tampered.InFlight.Argv = []string{"printf", "%s", "other"}
	if err := engine.Validate(tampered); err == nil || !strings.Contains(err.Error(), "bound inputs") {
		t.Fatal("tampered vector accepted", err)
	}
}

func TestUnbindableCommandEscalatesBeforeCharging(t *testing.T) {
	j, e := engine.New("task", 1, bindingWorkflow(), "tree")
	if e != nil {
		t.Fatal(e)
	}
	exec := engine.NewScripted(map[string][]engine.ScriptStep{"spec": {{Output: json.RawMessage(`{"acceptance":"-rf"}`)}}})
	j = drive(t, j, exec, false)
	if j.State != domain.Escalated || !strings.Contains(j.Reason, "CommandBinding") || j.Counts["run"] != 1 || j.Sequence != 1 || j.Attempts != 1 {
		t.Fatal("option-like binding dispatched", j.State, j.Reason, j.Counts)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// FAE-WF-044 acceptance: a workflow with an overlay for class "docs"
// dispatches its spec turn at the overlay's effort.
func TestTaskClassOverlayDispatchEffort(t *testing.T) {
	doc := bindingWorkflow()
	doc.TaskClasses = map[string]workflow.TaskClass{"docs": {Overlays: map[string]workflow.Overlay{"writer": {Effort: "low"}}}, "code": {}}
	doc.DefaultClass = "code"
	spec := func(class string) (engine.Job, *engine.Dispatch) {
		j, e := engine.New("task", 1, doc, "tree")
		if e == nil {
			j, e = engine.SetTaskClass(j, class)
		}
		if e != nil {
			t.Fatal(e)
		}
		for i := 0; i < 3; i++ {
			next, d, _, err := engine.Prepare(j)
			if err != nil {
				t.Fatal(err)
			}
			if j = next; d != nil {
				return j, d
			}
		}
		t.Fatal("no spec dispatch")
		return j, nil
	}
	j, d := spec("docs")
	if d.TaskClass != "docs" || d.Sessions[0].Route.Effort != "low" || engine.Validate(roundTrip(t, j)) != nil {
		t.Fatal("overlay effort not dispatched", d.Sessions[0].Route)
	}
	j, d = spec("")
	if j.TaskClass != "code" || d.Sessions[0].Route.Effort != "" {
		t.Fatal("default class", j.TaskClass, d.Sessions[0].Route)
	}
	j.InFlight.Sessions[0].Route.Effort = "low"
	if engine.Validate(roundTrip(t, j)) == nil {
		t.Fatal("overlay route accepted outside its class")
	}
	if _, err := engine.SetTaskClass(j, "ops"); err == nil {
		t.Fatal("unknown class accepted")
	}
}

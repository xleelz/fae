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

func requestWorkflow() workflow.Document {
	route := []workflow.Binding{{Provider: "script", Model: "recorded"}}
	return workflow.Document{SchemaVersion: 1, ID: "requests", Start: "work", Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 2},
		Roles: map[string]workflow.Role{
			"coder": {Instructions: "code", Briefs: map[string]string{"default": "brief"}, Routes: route, Scope: "task-bound", Cap: 1, Outputs: []string{"Report"}},
			"judge": {Instructions: "judge", Briefs: map[string]string{"default": "brief"}, Routes: route, Scope: "task-bound", Cap: 1, Outputs: []string{"Decision"}},
		},
		Artifacts: map[string]workflow.ArtifactType{"Report": {Version: 1, Schema: json.RawMessage(`{"type":"object","properties":{"request":{"type":"object"}}}`)}, "Decision": {Version: 1, Schema: json.RawMessage(`{"type":"object","properties":{"approved":{"type":"boolean"}},"required":["approved"]}`)}},
		Stages: map[string]workflow.Stage{
			"work":        {Primitive: workflow.AgentTurn, Role: "coder", Output: "Report"},
			"judge":       {Primitive: workflow.AgentTurn, Role: "judge", Output: "Decision", Inputs: []workflow.Input{{Name: "report", Type: "Report", Stage: "work", Version: 1}}},
			"widen-loop":  {Primitive: workflow.Loop, MaxIterations: 10},
			"rework-loop": {Primitive: workflow.Loop, MaxIterations: 10},
		},
		Edges:    []workflow.Edge{{From: "work", To: "Done"}, {From: "work", To: "judge"}, {From: "judge", To: "widen-loop"}, {From: "judge", To: "rework-loop"}, {From: "widen-loop", To: "work"}, {From: "rework-loop", To: "work"}},
		Requests: &workflow.RequestPolicy{From: []string{"work"}, Judge: "judge", Widen: "widen-loop", Rework: "rework-loop", ReworkCap: 1},
	}
}

func steps(outputs ...string) []engine.ScriptStep {
	out := []engine.ScriptStep{}
	for _, o := range outputs {
		out = append(out, engine.ScriptStep{Output: json.RawMessage(o)})
	}
	return out
}

const widen = `{"request":{"kind":"scope-widen","files":["pkg/extra.go"],"reason":"the fix touches the helper"}}`

func TestRequestValidation(t *testing.T) {
	if issues := workflow.Validate(requestWorkflow()); len(issues) != 0 {
		t.Fatal(issues)
	}
	for name, mutate := range map[string]func(*workflow.Document){
		"self judge":      func(d *workflow.Document) { s := d.Stages["judge"]; s.Role = "coder"; d.Stages["judge"] = s },
		"no judge edge":   func(d *workflow.Document) { d.Edges = slices.Delete(d.Edges, 1, 2) },
		"judge elsewhere": func(d *workflow.Document) { d.Edges = append(d.Edges, workflow.Edge{From: "judge", To: "Done"}) },
		"no rework cap":   func(d *workflow.Document) { d.Requests.ReworkCap = 0 },
		"undeclared field": func(d *workflow.Document) {
			d.Artifacts["Report"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","properties":{"x":{}}}`)}
		},
		"judge not boolean": func(d *workflow.Document) {
			d.Artifacts["Decision"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","properties":{"approved":{"type":"string"}}}`)}
		},
	} {
		d := requestWorkflow()
		mutate(&d)
		if len(workflow.Validate(d)) == 0 {
			t.Error(name, "accepted")
		}
	}
}

// FAE-WF-078: an approved widen is counted once, carried into every later
// dispatch across a restart, and its request and decision stay audited.
func TestScopeWidenApprovedCarriedThroughRestart(t *testing.T) {
	j, err := engine.New("task", 1, requestWorkflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	exec := engine.NewScripted(map[string][]engine.ScriptStep{"work": steps(widen, `{}`), "judge": steps(`{"approved":true}`)})
	var dispatches []engine.Dispatch
	for i := 0; i < 20 && !j.State.Terminal(); i++ {
		next, d, _, err := engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		j = roundTrip(t, next)
		if d == nil {
			continue
		}
		dispatches = append(dispatches, *d)
		r, err := exec.Execute(t.Context(), *d)
		if err != nil {
			t.Fatal(err)
		}
		if j, _, err = engine.Complete(j, d.ID, r); err != nil {
			t.Fatal(err)
		}
		j = roundTrip(t, j)
	}
	if j.State != domain.Done || !slices.Equal(j.ApprovedScope, []string{"pkg/extra.go"}) || j.RequestCounts["scope-widen"] != 1 || j.PendingRequest != nil {
		t.Fatal(j.State, j.Reason, j.ApprovedScope, j.RequestCounts)
	}
	if len(dispatches) != 3 || dispatches[1].Stage != "judge" || len(dispatches[0].Scope) != 0 || !slices.Equal(dispatches[2].Scope, []string{"pkg/extra.go"}) {
		t.Fatalf("%+v", dispatches)
	}
	kinds := []string{}
	for _, e := range j.History {
		if strings.HasPrefix(e.Kind, "request") {
			kinds = append(kinds, e.Kind)
		}
	}
	if !slices.Equal(kinds, []string{"request", "request-approved"}) {
		t.Fatal(kinds)
	}
	tampered := roundTrip(t, j)
	tampered.ApprovedScope = []string{"../escape"}
	if engine.Validate(tampered) == nil {
		t.Fatal("escaping scope accepted")
	}
}

func TestRequestRejectionExhaustionAndMalformed(t *testing.T) {
	run := func(script map[string][]engine.ScriptStep) engine.Job {
		j, err := engine.New("task", 1, requestWorkflow(), "tree")
		if err != nil {
			t.Fatal(err)
		}
		return drive(t, j, engine.NewScripted(script), true)
	}
	j := run(map[string][]engine.ScriptStep{"work": steps(widen), "judge": steps(`{"approved":false}`)})
	if j.EscalationKind != domain.ScopeWidenRejected || len(j.ApprovedScope) != 0 || j.RequestCounts["scope-widen"] != 1 {
		t.Fatal("rejected widen", j.EscalationKind, j.Reason, j.ApprovedScope)
	}
	j = run(map[string][]engine.ScriptStep{"work": steps(widen, widen, widen), "judge": steps(`{"approved":true}`, `{"approved":true}`)})
	if j.EscalationKind != domain.ScopeWidenRejected || !strings.Contains(j.Reason, "cap 2 exhausted") || j.RequestCounts["scope-widen"] != 2 || j.Counts["judge"] != 2 {
		t.Fatal("widen cap", j.EscalationKind, j.Reason, j.RequestCounts)
	}
	rework := `{"request":{"kind":"red-rework","reason":"the RED asserts the wrong value"}}`
	j = run(map[string][]engine.ScriptStep{"work": steps(rework, rework), "judge": steps(`{"approved":true}`)})
	if j.EscalationKind != domain.HumanDecisionRequired || !strings.Contains(j.Reason, "red-rework cap 1 exhausted") || j.Counts["rework-loop"] != 1 || j.Counts["widen-loop"] != 0 {
		t.Fatal("rework route and cap", j.EscalationKind, j.Reason, j.Counts)
	}
	for _, bad := range []string{`{"request":{"kind":"scope-widen","reason":"x"}}`, `{"request":{"kind":"scope-widen","files":["/etc/passwd"],"reason":"x"}}`, `{"request":{"kind":"bypass","reason":"x"}}`, `{"request":{"kind":"red-rework","reason":""}}`, `{"request":{"kind":"red-rework","reason":"x","approved":true}}`} {
		j = run(map[string][]engine.ScriptStep{"work": steps(bad, bad, bad, bad)})
		if j.EscalationKind != domain.SameGate || !strings.Contains(j.Reason, "MalformedOutput") || j.Counts["judge"] != 0 || len(j.RequestCounts) != 0 {
			t.Fatal("malformed request", bad, j.EscalationKind, j.Reason)
		}
	}
}

// FAE-WF-091 with FAE-WF-078: the simulator routes a request to its judge
// and an approval to the widen route, a rejection to Escalated.
func TestSimulateRequestRouting(t *testing.T) {
	record := func(stage, typ string, visit int, fields map[string]any) workflow.ScriptStep {
		return workflow.ScriptStep{Evidence: []workflow.Evidence{{ID: stage + string(rune('0'+visit)), TaskID: "task", Type: typ, Version: 1, Stage: stage, Provenance: true, Fields: fields}}}
	}
	request := map[string]any{"request": map[string]any{"kind": "scope-widen", "files": []any{"pkg/extra.go"}, "reason": "helper"}}
	for approved, want := range map[bool]domain.State{true: domain.Done, false: domain.Escalated} {
		script := workflow.Script{"work": {record("work", "Report", 1, request), record("work", "Report", 2, map[string]any{})}, "judge": {record("judge", "Decision", 1, map[string]any{"approved": approved})}}
		sim, err := engine.Simulate(requestWorkflow(), "task", script, workflow.SimulationOptions{})
		if err != nil || sim.State != want || !slices.Contains(sim.Path, "judge") {
			t.Fatal(approved, sim.State, sim.Path, sim.Reason, err)
		}
		if !approved && !strings.HasPrefix(sim.Reason, "ScopeWidenRejected") {
			t.Fatal(sim.Reason)
		}
	}
}

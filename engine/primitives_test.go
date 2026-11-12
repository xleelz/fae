package engine_test

import (
	"encoding/json"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func lit(v any) workflow.Expr { return workflow.Expr{Op: "literal", Value: v} }
func bin(op string, a, b workflow.Expr) workflow.Expr {
	return workflow.Expr{Op: op, Args: []workflow.Expr{a, b}}
}
func fieldOf(stage, path string) workflow.Expr {
	return workflow.Expr{Op: "field", Stage: stage, Path: []string{path}}
}

// floorDoc is a minimal protected graph: change, test run, test gate,
// independent review, commit. Primitive tests splice their stage in before
// the commit.
func floorDoc() workflow.Document {
	role := func() workflow.Role {
		return workflow.Role{Instructions: "instructions", Briefs: map[string]string{"default": "brief"}, Routes: []workflow.Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"report"}}
	}
	gate := bin("and", bin("eq", fieldOf("test", "exit_code"), lit(0)), bin("ge", fieldOf("test", "executed_count"), lit(1)))
	return workflow.Document{SchemaVersion: 1, ID: "primitives", Start: "change", Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 3},
		Roles:     map[string]workflow.Role{"coder": role(), "reviewer": role()},
		Artifacts: map[string]workflow.ArtifactType{"report": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}},
		Stages: map[string]workflow.Stage{
			"change": {Primitive: workflow.AgentTurn, Role: "coder", Output: "report", ChangeProducer: true},
			"test":   {Primitive: workflow.HostCommand, Output: "report", Command: []string{"go", "test", "-json", "-count=1", "./..."}, Weight: 1, TimeoutSeconds: 30, TestRun: true},
			"gate":   {Primitive: workflow.Gate, Expr: &gate, TestStage: "test", Failure: "stop"},
			"review": {Primitive: workflow.Review, Role: "reviewer", Output: "report"},
			"commit": {Primitive: workflow.VcsCommit, Output: "report", Protected: true},
			"stop":   {Primitive: workflow.Escalate},
		},
		Edges: []workflow.Edge{{From: "change", To: "test"}, {From: "test", To: "gate"}, {From: "gate", To: "review"}, {From: "gate", To: "stop"}, {From: "review", To: "commit"}, {From: "commit", To: "Done"}},
	}
}

func step(output string) []engine.ScriptStep {
	return []engine.ScriptStep{{Output: json.RawMessage(output)}}
}
func floorScript(visits int) map[string][]engine.ScriptStep {
	script := map[string][]engine.ScriptStep{}
	for i := 0; i < visits; i++ {
		script["change"] = append(script["change"], engine.ScriptStep{Output: json.RawMessage(`{"change":` + string(rune('0'+i)) + `}`)})
		script["test"] = append(script["test"], step(`{"exit_code":0,"executed_count":1,"fresh_build":true}`)...)
		script["review"] = append(script["review"], engine.ScriptStep{Reviews: []json.RawMessage{json.RawMessage(`{"approved":true}`)}})
	}
	script["commit"] = step(`{"sha":"scripted"}`)
	return script
}

// replaceEdge swaps one edge for others.
func replaceEdge(d *workflow.Document, old workflow.Edge, with ...workflow.Edge) {
	edges := []workflow.Edge{}
	for _, e := range d.Edges {
		if e != old {
			edges = append(edges, e)
		}
	}
	d.Edges = append(edges, with...)
}

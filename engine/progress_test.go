package engine_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func progressWorkflow(progress *workflow.Progress) workflow.Document {
	route := []workflow.Binding{{Provider: "script", Model: "recorded"}}
	blocking := workflow.Expr{Op: "count", Stage: "review", Path: []string{"findings"}, Args: []workflow.Expr{{Op: "eq", Args: []workflow.Expr{{Op: "item", Path: []string{"severity"}}, {Op: "literal", Value: "blocking"}}}}}
	pass := workflow.Expr{Op: "and", Args: []workflow.Expr{{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "review", Path: []string{"approved"}}, {Op: "literal", Value: true}}}, {Op: "eq", Args: []workflow.Expr{blocking, {Op: "literal", Value: 0}}}}}
	if progress != nil {
		progress.Measure = blocking
	}
	return workflow.Document{SchemaVersion: 1, ID: "progress", Start: "work", Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 3},
		Roles: map[string]workflow.Role{
			"coder":    {Instructions: "code", Briefs: map[string]string{"default": "brief"}, Routes: route, Scope: "task-bound", Cap: 1, Outputs: []string{"Report"}},
			"reviewer": {Instructions: "review", Briefs: map[string]string{"default": "brief"}, Routes: route, Scope: "task-bound", Cap: 1, Outputs: []string{"Review"}},
		},
		Artifacts: map[string]workflow.ArtifactType{"Report": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}, "Review": {Version: 1, Schema: json.RawMessage(`{"type":"object","properties":{"approved":{"type":"boolean"},"findings":{"type":"array"}},"required":["approved","findings"]}`)}},
		Stages: map[string]workflow.Stage{
			"work":   {Primitive: workflow.AgentTurn, Role: "coder", Output: "Report"},
			"review": {Primitive: workflow.Review, Role: "reviewer", Output: "Review"},
			"gate":   {Primitive: workflow.Gate, Expr: &pass, Failure: "loop"},
			"loop":   {Primitive: workflow.Loop, MaxIterations: 2, Progress: progress},
		},
		Edges: []workflow.Edge{{From: "work", To: "review"}, {From: "review", To: "gate"}, {From: "gate", To: "Done"}, {From: "gate", To: "loop"}, {From: "loop", To: "work"}},
	}
}

func reviewsWith(blocking ...int) []engine.ScriptStep {
	out := []engine.ScriptStep{}
	for _, n := range blocking {
		findings := []map[string]string{}
		for i := 0; i < n; i++ {
			findings = append(findings, map[string]string{"finding": fmt.Sprint(i), "severity": "blocking"})
		}
		b, _ := json.Marshal(map[string]any{"approved": n == 0, "findings": findings})
		out = append(out, engine.ScriptStep{Reviews: []json.RawMessage{b}})
	}
	return out
}

// FAE-WF-075 acceptance: a remediation loop reducing blocking findings
// 5 -> 3 -> 1 -> 0 finishes with a loop cap of 2; without the declaration it
// exhausts the cap, and a no-improvement streak past its bound escalates.
func TestProgressAwareLoopDiscount(t *testing.T) {
	run := func(progress *workflow.Progress, blocking ...int) engine.Job {
		doc := progressWorkflow(progress)
		if issues := workflow.Validate(doc); len(issues) != 0 {
			t.Fatal(issues)
		}
		j, err := engine.New("task", 1, doc, "tree")
		if err != nil {
			t.Fatal(err)
		}
		work := []engine.ScriptStep{}
		for range blocking {
			work = append(work, engine.ScriptStep{Output: json.RawMessage(`{}`)})
		}
		return drive(t, j, engine.NewScripted(map[string][]engine.ScriptStep{"work": work, "review": reviewsWith(blocking...)}), true)
	}
	j := run(&workflow.Progress{Streak: 1}, 5, 3, 1, 0)
	if j.State != domain.Done || j.Counts["loop"] != 1 {
		t.Fatal("converging loop did not finish", j.State, j.Reason, j.Counts)
	}
	discounts := 0
	for _, e := range j.History {
		if e.Kind == "loop-progress" {
			discounts++
		}
	}
	if discounts != 2 {
		t.Fatal("discounted iterations not audited", discounts)
	}
	if j = run(nil, 5, 3, 1, 0); j.EscalationKind != domain.LoopCap {
		t.Fatal("undeclared progress discounted", j.State, j.Reason)
	}
	if j = run(&workflow.Progress{Streak: 1}, 5, 5, 5, 0); j.EscalationKind != domain.LoopCap || !strings.Contains(j.Reason, "no progress for 2 iterations, over its bound of 1; the streak carries for the life of the task") {
		t.Fatal("no-improvement streak unbounded", j.State, j.Reason)
	}
	bad := progressWorkflow(&workflow.Progress{Streak: 0})
	s := bad.Stages["gate"]
	s.Progress = &workflow.Progress{Measure: workflow.Expr{Op: "literal", Value: 1}, Streak: 1}
	bad.Stages["gate"] = s
	issues := workflow.Validate(bad)
	paths := map[string]bool{}
	for _, i := range issues {
		paths[i.Path] = true
	}
	if !paths["stages.loop.progress.streak"] || !paths["stages.gate.progress"] || !paths["stages.gate.progress.measure"] {
		t.Fatal(issues)
	}
}

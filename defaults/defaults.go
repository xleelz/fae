// Package defaults supplies version-one local scripted workflow content. The
// graph is interpreted by engine; no engine state names are hard-coded here.
package defaults

import (
	"encoding/json"
	"sort"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func literal(v any) workflow.Expr { return workflow.Expr{Op: "literal", Value: v} }
func field(stage, path string) workflow.Expr {
	return workflow.Expr{Op: "field", Stage: stage, Path: []string{path}}
}
func bin(op string, a, b workflow.Expr) workflow.Expr {
	return workflow.Expr{Op: op, Args: []workflow.Expr{a, b}}
}
func eq(stage, path string, v any) workflow.Expr { return bin("eq", field(stage, path), literal(v)) }
func and(xs ...workflow.Expr) workflow.Expr {
	x := xs[0]
	for _, b := range xs[1:] {
		x = bin("and", x, b)
	}
	return x
}
func schema(properties map[string]any) json.RawMessage {
	required := []string{}
	for k := range properties {
		required = append(required, k)
	}
	sort.Strings(required)
	b, _ := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
	return b
}
func typ(t string) map[string]any          { return map[string]any{"type": t} }
func enum(values ...string) map[string]any { return map[string]any{"type": "string", "enum": values} }
func artifact(p map[string]any) workflow.ArtifactType {
	return workflow.ArtifactType{Version: 1, Schema: schema(p)}
}
func input(name, kind, stage string) workflow.Input {
	return workflow.Input{Name: name, Type: kind, Stage: stage, Version: 1}
}
func gate(x workflow.Expr, failure string) workflow.Stage {
	return workflow.Stage{Primitive: workflow.Gate, Expr: &x, Failure: failure}
}
func run() workflow.Stage {
	return workflow.Stage{Primitive: workflow.HostCommand, Output: "EngineRun", Command: []string{"go", "test", "-json", "-count=1", "./..."}, Weight: 1, TimeoutSeconds: 60, TestRun: true}
}

// Workflow returns independent content; routes name rehearsal providers until an
// operator installs actual provider bindings. Normal external effects are absent
// because no live granted/pinned effect executor is implemented yet.
func Workflow() workflow.Document {
	d := workflow.Document{SchemaVersion: 1, CodeBearing: true, ID: "default", Start: "intake", Roles: map[string]workflow.Role{}, Artifacts: map[string]workflow.ArtifactType{}, Stages: map[string]workflow.Stage{}, Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 3}}
	roles := []struct {
		name           string
		cap            int
		scope, purpose string
	}{
		{"spec", 2, "workflow-global", "Turn the item into a precise testable spec with scope, acceptance criteria, contracts, risks and required test intent. Do not write or format code or run builds or tests."},
		{"spec-reviewer", 2, "task-bound", "Adversarially check ambiguity, untestability, contracts and scope. Approve or reject with findings; do not rewrite the spec, write code or run builds."},
		{"planner", 2, "workflow-global", "Produce ordered steps, declared files, test placement and risks. Do not write code or run builds."},
		{"coder", 4, "task-bound", "Test-first: author only declared tests. Implement: make the frozen RED pass with minimal clean code and proof. Do not edit frozen RED, widen scope silently or touch outside the worktree."},
		{"reviewer-correctness", 4, "task-bound", "Independently review correctness and record findings. Do not edit code or review other dimensions."},
		{"reviewer-quality", 4, "task-bound", "Independently review quality and record findings. Do not edit code or review other dimensions."},
		{"reviewer-security", 4, "task-bound", "Independently review security and record findings. Do not edit code or review other dimensions."},
		{"reviewer-architecture", 4, "task-bound", "Independently review architecture and record findings. Do not edit code or review other dimensions."},
		{"oracle", 1, "workflow-global", "Decide acceptance with typed validity, severity and target for findings. Do not edit code or soften blocking findings."},
		{"qa", 1, "workflow-global", "Interpret required engine runs and checks on the task tree. Do not edit source or skip tests."},
	}
	for _, r := range roles {
		d.Roles[r.name] = workflow.Role{Instructions: r.purpose, Briefs: map[string]string{"main": "Evaluate bound typed artifacts and produce the required output."}, Routes: []workflow.Binding{{Provider: "scripted-a", Model: "primary"}, {Provider: "scripted-b", Model: "secondary"}, {Provider: "scripted-a", Model: "fallback"}, {Provider: "scripted-b", Model: "last"}}, Scope: r.scope, Cap: r.cap, Profile: workflow.Profile{Allowed: []string{"handoff"}, Denied: []string{"external-effect"}}}
	}
	str := map[string]any{"type": "string", "minLength": 1}
	boolean := typ("boolean")
	integer := typ("integer")
	strings := map[string]any{"type": "array", "items": str, "minItems": 1}
	d.Artifacts["Intake"] = artifact(map[string]any{"accepted": boolean})
	d.Artifacts["Spec"] = artifact(map[string]any{"scope": str, "acceptance": strings, "contracts": strings, "risks": strings, "required_tests": strings, "declared_files": strings})
	d.Artifacts["SpecReview"] = artifact(map[string]any{"approved": boolean, "findings": map[string]any{"type": "array", "items": str}})
	d.Artifacts["Plan"] = artifact(map[string]any{"steps": strings, "declared_files": strings, "test_placement": str, "risks": strings})
	d.Artifacts["RedReport"] = artifact(map[string]any{"test_id": str, "test_shape": enum("go-unit"), "declared_files": strings, "expected_failure": str, "source": str})
	d.Artifacts["EngineRun"] = artifact(map[string]any{"exit_code": integer, "executed_count": map[string]any{"type": "integer", "minimum": 0}, "matched_count": integer, "expected_failure": boolean, "test_in_source": boolean, "red_passed": boolean, "fresh_build": boolean, "output_digest": str})
	d.Artifacts["FrozenIdentity"] = artifact(map[string]any{"identity": str})
	d.Artifacts["Compare"] = artifact(map[string]any{"identity": str, "unchanged": boolean})
	d.Artifacts["ImplementationReport"] = artifact(map[string]any{"changed_files": strings, "hunks": strings, "proof": str, "source": str, "test_id": str})
	d.Artifacts["Format"] = artifact(map[string]any{"new_debt": integer})
	d.Artifacts["Review"] = artifact(map[string]any{"approved": boolean, "dimension": enum("correctness", "quality", "security", "architecture"), "findings": map[string]any{"type": "array", "items": str}})
	d.Artifacts["OracleVerdict"] = artifact(map[string]any{"decision": enum("pass", "RedTest", "Production", "unknown"), "valid_blocking": map[string]any{"type": "integer", "minimum": 0}, "rationale": str})
	d.Artifacts["ReconcileReport"] = artifact(map[string]any{"result": enum("clean", "conflicted", "own-hunk-loss"), "patch_identical": boolean})
	// Conflict metadata is optional for backward-compatible clean report fixtures.
	reconcile := d.Artifacts["ReconcileReport"]
	var reportSchema map[string]any
	_ = json.Unmarshal(reconcile.Schema, &reportSchema)
	props := reportSchema["properties"].(map[string]any)
	props["paths"] = map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "minItems": 1, "uniqueItems": true}
	for _, key := range []string{"original_base", "original_tree", "base", "plan_key"} {
		props[key] = map[string]any{"type": "string", "minLength": 1}
	}
	reconcile.Schema, _ = json.Marshal(reportSchema)
	d.Artifacts["ReconcileReport"] = reconcile
	d.Artifacts["Commit"] = artifact(map[string]any{"sha": str, "files": strings})
	d.Artifacts["Integration"] = artifact(map[string]any{"landed": boolean, "sha": str})
	add := func(id string, s workflow.Stage, next string) {
		d.Stages[id] = s
		if next != "" {
			d.Edges = append(d.Edges, workflow.Edge{From: id, To: next})
		}
	}
	edge := func(from, to string) { d.Edges = append(d.Edges, workflow.Edge{From: from, To: to}) }
	agent := func(role, output string, ins ...workflow.Input) workflow.Stage {
		return workflow.Stage{Primitive: workflow.AgentTurn, Role: role, Output: output, Inputs: ins}
	}
	add("intake", workflow.Stage{Primitive: workflow.AgentTurn, Role: "spec", Output: "Intake"}, "intake-gate")
	add("intake-gate", gate(eq("intake", "accepted", true), "stop"), "spec-loop")
	edge("intake-gate", "stop")
	add("spec-loop", workflow.Stage{Primitive: workflow.Loop, MaxIterations: 3}, "spec")
	add("spec", agent("spec", "Spec", input("intake", "Intake", "intake")), "spec-review")
	add("spec-review", workflow.Stage{Primitive: workflow.Review, Role: "spec-reviewer", Count: 2, Output: "SpecReview", Inputs: []workflow.Input{input("spec", "Spec", "spec")}}, "spec-gate")
	// Count the current review batch in engine gate selection; all reviewers must approve.
	specPass := bin("eq", workflow.Expr{Op: "count", Stage: "spec-review", Args: []workflow.Expr{eq("spec-review", "approved", true)}}, literal(2))
	add("spec-gate", gate(specPass, "spec-loop"), "plan")
	edge("spec-gate", "spec-loop")
	add("plan", agent("planner", "Plan", input("spec", "Spec", "spec"), input("review", "SpecReview", "spec-review")), "red-loop")
	add("red-loop", workflow.Stage{Primitive: workflow.Loop, MaxIterations: 8}, "test-first")
	test := agent("coder", "RedReport", input("plan", "Plan", "plan"))
	test.ChangeProducer = true
	test.Phase = "test-first"
	add("test-first", test, "run-red")
	red := run()
	red.Inputs = []workflow.Input{input("red", "RedReport", "test-first")}
	add("run-red", red, "red-gate")
	redPass := and(bin("ne", field("run-red", "exit_code"), literal(0)), bin("ne", field("run-red", "exit_code"), literal(127)), eq("run-red", "matched_count", 1), eq("run-red", "executed_count", 1), eq("run-red", "expected_failure", true), eq("run-red", "test_in_source", true))
	add("red-gate", gate(redPass, "red-loop"), "freeze-red")
	edge("red-gate", "red-loop")
	add("freeze-red", workflow.Stage{Primitive: workflow.FreezeArtifact, Output: "FrozenIdentity", Subject: "test-first", Inputs: []workflow.Input{input("red", "RedReport", "test-first"), input("run", "EngineRun", "run-red")}}, "implement-loop")
	add("implement-loop", workflow.Stage{Primitive: workflow.Loop, MaxIterations: 8}, "implement")
	implement := agent("coder", "ImplementationReport", input("plan", "Plan", "plan"), input("frozen", "FrozenIdentity", "freeze-red"))
	conflictInput := input("reconciliation", "ReconcileReport", "merge")
	conflictInput.Optional = true
	implement.Inputs = append(implement.Inputs, conflictInput)
	implement.ChangeProducer = true
	implement.Phase = "implement"
	add("implement", implement, "run-tests")
	green := run()
	green.Inputs = []workflow.Input{input("implementation", "ImplementationReport", "implement")}
	add("run-tests", green, "compare-red")
	add("compare-red", workflow.Stage{Primitive: workflow.CompareFrozen, Output: "Compare", Subject: "implement", Inputs: []workflow.Input{input("frozen", "FrozenIdentity", "freeze-red"), input("implementation", "ImplementationReport", "implement")}}, "green-gate")
	greenPass := and(eq("run-tests", "exit_code", 0), bin("ge", field("run-tests", "executed_count"), literal(1)), eq("run-tests", "red_passed", true), eq("compare-red", "unchanged", true))
	gg := gate(greenPass, "implement-loop")
	gg.TestStage = "run-tests"
	add("green-gate", gg, "format")
	edge("green-gate", "implement-loop")
	add("format", workflow.Stage{Primitive: workflow.HostCommand, Output: "Format", Command: []string{"gofmt", "-l", "."}, Weight: 1, TimeoutSeconds: 60}, "format-gate")
	add("format-gate", gate(eq("format", "new_debt", 0), "implement-loop"), "reviews")
	edge("format-gate", "implement-loop")
	add("reviews", workflow.Stage{Primitive: workflow.Review, Role: "reviewer-correctness", Reviewers: []string{"reviewer-correctness", "reviewer-quality", "reviewer-security", "reviewer-architecture"}, Output: "Review", Inputs: []workflow.Input{input("implementation", "ImplementationReport", "implement"), input("green", "EngineRun", "run-tests")}}, "reviews-gate")
	reviewPass := bin("eq", workflow.Expr{Op: "count", Stage: "reviews", Args: []workflow.Expr{eq("reviews", "approved", true)}}, literal(4))
	for _, dimension := range []string{"correctness", "quality", "security", "architecture"} {
		reviewPass = and(reviewPass, bin("eq", workflow.Expr{Op: "count", Stage: "reviews", Args: []workflow.Expr{and(eq("reviews", "approved", true), eq("reviews", "dimension", dimension))}}, literal(1)))
	}
	add("reviews-gate", gate(reviewPass, "implement-loop"), "oracle")
	edge("reviews-gate", "implement-loop")
	add("oracle", agent("oracle", "OracleVerdict", input("reviews", "Review", "reviews")), "verdict")
	add("verdict", workflow.Stage{Primitive: workflow.Branch, Default: "stop", Cases: []workflow.Case{{When: eq("oracle", "decision", "RedTest"), Next: "red-loop"}, {When: eq("oracle", "decision", "Production"), Next: "implement-loop"}, {When: and(eq("oracle", "decision", "pass"), eq("oracle", "valid_blocking", 0)), Next: "qa"}}}, "stop")
	edge("verdict", "red-loop")
	edge("verdict", "implement-loop")
	edge("verdict", "qa")
	qa := run()
	qa.Weight = 2
	qa.Inputs = []workflow.Input{input("oracle", "OracleVerdict", "oracle")}
	add("qa", qa, "qa-gate")
	qg := gate(and(eq("qa", "exit_code", 0), bin("ge", field("qa", "executed_count"), literal(1))), "implement-loop")
	qg.TestStage = "qa"
	add("qa-gate", qg, "merge")
	edge("qa-gate", "implement-loop")
	add("merge", workflow.Stage{Primitive: workflow.Merge, Output: "ReconcileReport", Reverify: "reverify-loop"}, "merge-route")
	edge("merge", "reverify-loop")
	add("reverify-loop", workflow.Stage{Primitive: workflow.Loop, MaxIterations: 3}, "run-tests")
	add("merge-route", workflow.Stage{Primitive: workflow.Branch, Default: "stop", Cases: []workflow.Case{{When: and(eq("merge", "result", "clean"), eq("merge", "patch_identical", true)), Next: "post-merge-qa"}, {When: eq("merge", "result", "conflicted"), Next: "implement-loop"}}}, "stop")
	edge("merge-route", "post-merge-qa")
	edge("merge-route", "implement-loop")
	add("post-merge-qa", qa, "post-merge-gate")
	pg := gate(and(eq("post-merge-qa", "exit_code", 0), bin("ge", field("post-merge-qa", "executed_count"), literal(1))), "implement-loop")
	pg.TestStage = "post-merge-qa"
	add("post-merge-gate", pg, "commit")
	edge("post-merge-gate", "implement-loop")
	add("commit", workflow.Stage{Primitive: workflow.VcsCommit, Protected: true, Output: "Commit", Inputs: []workflow.Input{input("implementation", "ImplementationReport", "implement"), input("frozen-check", "Compare", "compare-red"), input("qa", "EngineRun", "post-merge-qa")}}, "integrate")
	add("integrate", workflow.Stage{Primitive: workflow.Integrate, Reverify: "integration-retry-loop", Protected: true, Output: "Integration", Inputs: []workflow.Input{input("commit", "Commit", "commit")}}, "integration-gate")
	edge("integrate", "integration-retry-loop")
	add("integration-retry-loop", workflow.Stage{Primitive: workflow.Loop, MaxIterations: 3}, "merge")
	add("integration-gate", gate(and(workflow.Expr{Op: "exists", Stage: "commit"}, eq("integrate", "landed", true), bin("eq", field("integrate", "sha"), field("commit", "sha"))), "stop"), "Done")
	edge("integration-gate", "stop")
	add("stop", workflow.Stage{Primitive: workflow.Escalate}, "")
	for id, r := range d.Roles {
		permitted := map[string]bool{}
		for _, stage := range d.Stages {
			if stage.Role == id {
				permitted[stage.Output] = true
			}
			for _, reviewer := range stage.Reviewers {
				if reviewer == id {
					permitted[stage.Output] = true
				}
			}
		}
		for name := range permitted {
			if name != "" {
				r.Outputs = append(r.Outputs, name)
			}
		}
		sort.Strings(r.Outputs)
		d.Roles[id] = r
	}
	return d
}

const redSource = "package example\nfunc TestBehavior(t *T) { assert(answer() == 42) }\n"

func raw(x any) json.RawMessage { b, _ := json.Marshal(x); return b }

// HappyScript explicitly describes a local rehearsal. It is never used as an
// attestation that real tests, commits, or integration occurred.
func HappyScript() map[string][]engine.ScriptStep {
	output := func(v any) []engine.ScriptStep { return []engine.ScriptStep{{Output: raw(v)}} }
	run := func(exit int, red bool) map[string]any {
		return map[string]any{"exit_code": exit, "executed_count": 1, "matched_count": 1, "expected_failure": exit != 0, "test_in_source": true, "red_passed": red, "fresh_build": true, "output_digest": "scripted-rehearsal"}
	}
	review := func(dimension string) json.RawMessage {
		return raw(map[string]any{"approved": true, "dimension": dimension, "findings": []string{}})
	}
	return map[string][]engine.ScriptStep{
		"intake":      output(map[string]any{"accepted": true}),
		"spec":        output(map[string]any{"scope": "scripted rehearsal", "acceptance": []string{"answer is 42"}, "contracts": []string{"answer() int"}, "risks": []string{"none in fixture"}, "required_tests": []string{"TestBehavior"}, "declared_files": []string{"answer.go", "answer_test.go"}}),
		"spec-review": {{Reviews: []json.RawMessage{raw(map[string]any{"approved": true, "findings": []string{}}), raw(map[string]any{"approved": true, "findings": []string{}})}}},
		"plan":        output(map[string]any{"steps": []string{"test", "implement"}, "declared_files": []string{"answer.go", "answer_test.go"}, "test_placement": "answer_test.go", "risks": []string{"none in fixture"}}),
		"test-first":  output(map[string]any{"test_id": "TestBehavior", "test_shape": "go-unit", "declared_files": []string{"answer_test.go"}, "expected_failure": "got 0 want 42", "source": redSource}),
		"run-red":     output(run(1, false)), "freeze-red": output(map[string]any{}),
		"implement": output(map[string]any{"changed_files": []string{"answer.go"}, "hunks": []string{"return 42"}, "proof": "scripted fixture", "source": redSource, "test_id": "TestBehavior"}),
		"run-tests": output(run(0, true)), "compare-red": output(map[string]any{}), "format": output(map[string]any{"new_debt": 0}),
		"reviews": {{Reviews: []json.RawMessage{review("correctness"), review("quality"), review("security"), review("architecture")}}},
		"oracle":  output(map[string]any{"decision": "pass", "valid_blocking": 0, "rationale": "all fixture findings resolved"}),
		"qa":      output(run(0, true)), "merge": output(map[string]any{"result": "clean", "patch_identical": true}), "post-merge-qa": output(run(0, true)),
		"commit": output(map[string]any{"sha": "scripted-commit", "files": []string{"answer.go", "answer_test.go"}}), "integrate": output(map[string]any{"landed": true, "sha": "scripted-commit"}),
	}
}

package workflow

import (
	"encoding/json"
	"testing"
)

// noRedDocument is a code-bearing graph with a RED route and a route only a
// spec-declared no-red task can take.
func noRedDocument() Document {
	d := protected()
	role := func(outputs ...string) Role {
		return Role{Instructions: "instruction", Briefs: map[string]string{"default": "brief"}, Routes: []Binding{{Provider: "script", Model: "recorded", Effort: "high"}}, Scope: "task-bound", Cap: 1, Outputs: outputs}
	}
	d.Roles = map[string]Role{"writer": role("Spec"), "coder": role("report"), "reviewer": role("report")}
	d.Artifacts["Spec"] = ArtifactType{1, json.RawMessage(`{"type":"object","properties":{"no_red":{"type":"boolean"}}}`)}
	run := func(shape string) Stage {
		return Stage{Primitive: HostCommand, Output: "report", Command: []string{"go", "test", "./..."}, Weight: 1, TimeoutSeconds: 30, TestRun: true, TestShape: shape}
	}
	redPass := bin("and", bin("and", bin("and", bin("ne", field("run-red", "exit_code"), lit(0)), bin("ne", field("run-red", "exit_code"), lit(127))), bin("and", bin("eq", field("run-red", "matched_count"), lit(1)), bin("eq", field("run-red", "executed_count"), lit(1)))), bin("and", bin("eq", field("run-red", "expected_failure"), lit(true)), bin("eq", field("run-red", "test_in_source"), lit(true))))
	green := bin("and", bin("and", bin("eq", field("run", "exit_code"), lit(0)), bin("ge", field("run", "executed_count"), lit(1))), bin("and", bin("eq", field("run", "red_passed"), lit(true)), bin("eq", field("compare", "unchanged"), lit(true))))
	suite := bin("and", bin("eq", field("suite", "exit_code"), lit(0)), bin("ge", field("suite", "executed_count"), lit(1)))
	noRed := bin("eq", field("spec", "no_red"), lit(true))
	d.Start = "spec"
	d.NoRedStages = []string{"spec"}
	d.Stages = map[string]Stage{
		"spec":       {Primitive: AgentTurn, Role: "writer", Output: "Spec"},
		"route":      {Primitive: Branch, Default: "red", Cases: []Case{{When: noRed, Next: "impl-nored"}}},
		"red":        {Primitive: AgentTurn, Role: "coder", Output: "report", ChangeProducer: true, Phase: "test-first"},
		"run-red":    run("go-unit"),
		"red-gate":   {Primitive: Gate, Expr: &redPass, Failure: "stop"},
		"freeze":     {Primitive: FreezeArtifact, Output: "report"},
		"impl":       {Primitive: AgentTurn, Role: "coder", Output: "report", ChangeProducer: true, Phase: "implement"},
		"run":        run(""),
		"compare":    {Primitive: CompareFrozen, Output: "report"},
		"gate":       {Primitive: Gate, Expr: &green, TestStage: "run", Failure: "stop"},
		"impl-nored": {Primitive: AgentTurn, Role: "coder", Output: "report", ChangeProducer: true, Phase: "implement"},
		"suite":      run(""),
		"suite-gate": {Primitive: Gate, Expr: &suite, TestStage: "suite", Failure: "stop"},
		"review":     {Primitive: Review, Role: "reviewer", Output: "report"},
		"commit":     {Primitive: VcsCommit, Output: "report", Protected: true},
		"stop":       {Primitive: Escalate},
	}
	d.Edges = []Edge{{"spec", "route"}, {"route", "impl-nored"}, {"route", "red"}, {"red", "run-red"}, {"run-red", "red-gate"}, {"red-gate", "freeze"}, {"red-gate", "stop"}, {"freeze", "impl"}, {"impl", "run"}, {"run", "compare"}, {"compare", "gate"}, {"gate", "review"}, {"gate", "stop"}, {"impl-nored", "suite"}, {"suite", "suite-gate"}, {"suite-gate", "review"}, {"suite-gate", "stop"}, {"review", "commit"}, {"commit", "Done"}}
	d.TaskClasses = map[string]TaskClass{"code": {CodeBearing: true}, "docs": {Overlays: map[string]Overlay{"writer": {Effort: "low", Briefs: map[string]string{"default": "docs brief"}}}}}
	d.DefaultClass = "code"
	return d
}

// FAE-WF-073: only an arm asserting the spec's no-red declaration may reach
// implementation without RED; the implementing role can never declare it.
func TestNoRedRouteValidation(t *testing.T) {
	if issues := Validate(noRedDocument()); len(issues) != 0 {
		t.Fatal(issues)
	}
	// Another provider's minimal effort is not refused.
	minimal := noRedDocument()
	minimal.TaskClasses["docs"].Overlays["writer"] = Overlay{Effort: "minimal"}
	if issues := Validate(minimal); len(issues) != 0 {
		t.Fatal(issues)
	}
	cases := map[string]struct {
		path, message string
		mutate        func(*Document)
	}{
		"undeclared": {"stages.impl-nored", "lacks frozen RED", func(d *Document) { d.NoRedStages = nil }},
		"arm asserts something else": {"stages.impl-nored", "lacks frozen RED", func(d *Document) {
			s := d.Stages["route"]
			s.Cases = []Case{{When: bin("eq", field("spec", "no_red"), lit(false)), Next: "impl-nored"}}
			d.Stages["route"] = s
		}},
		"default arm skips RED": {"stages.impl-nored", "lacks frozen RED", func(d *Document) {
			s := d.Stages["route"]
			s.Default, s.Cases = "impl-nored", []Case{{When: bin("eq", field("spec", "no_red"), lit(false)), Next: "red"}}
			d.Stages["route"] = s
		}},
		"implementing role declares": {"no_red_stages[0]", "never an implementing role", func(d *Document) { d.NoRedStages = []string{"impl"} }},
		"writer also implements": {"no_red_stages[0]", "never an implementing role", func(d *Document) {
			s := d.Stages["impl-nored"]
			s.Role = "writer"
			d.Stages["impl-nored"] = s
			r := d.Roles["writer"]
			r.Outputs = append(r.Outputs, "report")
			d.Roles["writer"] = r
		}},
		"undeclarable artifact": {"no_red_stages[0]", "boolean no_red", func(d *Document) {
			d.Artifacts["Spec"] = ArtifactType{1, json.RawMessage(`{"type":"object","properties":{"scope":{"type":"string"}}}`)}
		}},
		"not code-bearing": {"no_red_stages", "code-bearing", func(d *Document) { d.TaskClasses = nil; d.DefaultClass = "" }},
		"unknown default":  {"default_class", "unknown task class", func(d *Document) { d.DefaultClass = "ops" }},
		"overlay role":     {"task_classes.docs.overlays.ghost", "unknown role", func(d *Document) { d.TaskClasses["docs"].Overlays["ghost"] = Overlay{Effort: "low"} }},
		"overlay effort":   {"task_classes.docs.overlays.writer.effort", "unknown effort", func(d *Document) { d.TaskClasses["docs"].Overlays["writer"] = Overlay{Effort: "max"} }},
		"overlay brief": {"task_classes.docs.overlays.writer.briefs.review", "declared phase", func(d *Document) {
			d.TaskClasses["docs"].Overlays["writer"] = Overlay{Briefs: map[string]string{"review": "x"}}
		}},
		"empty overlay": {"task_classes.docs.overlays.writer", "overrides nothing", func(d *Document) { d.TaskClasses["docs"].Overlays["writer"] = Overlay{} }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := noRedDocument()
			c.mutate(&d)
			if issues := Validate(d); !hasIssue(issues, c.path, c.message) {
				t.Fatal("accepted or wrong path", issues)
			}
		})
	}
}

// A code-bearing class alone, without the legacy document flag, puts the
// graph under the RED floor (FAE-WF-073 per task class).
func TestCodeBearingTaskClassRequiresRed(t *testing.T) {
	d := noRedDocument()
	d.NoRedStages = nil
	s := d.Stages["route"]
	s.Cases = nil
	s.Default = "impl-nored"
	d.Stages["route"] = s
	d.Edges = append(d.Edges[:2], d.Edges[3:]...)
	d.Stages = func() map[string]Stage {
		out := map[string]Stage{}
		for id, s := range d.Stages {
			if id != "red" && id != "run-red" && id != "red-gate" && id != "freeze" && id != "impl" && id != "run" && id != "compare" && id != "gate" {
				out[id] = s
			}
		}
		return out
	}()
	var edges []Edge
	for _, e := range d.Edges {
		if _, ok := d.Stages[e.From]; ok {
			edges = append(edges, e)
		}
	}
	d.Edges = edges
	if issues := Validate(d); !hasIssue(issues, "stages.impl-nored", "lacks frozen RED") {
		t.Fatal("code-bearing class without RED accepted", issues)
	}
	d.TaskClasses = map[string]TaskClass{"docs": {}}
	d.DefaultClass = "docs"
	if issues := Validate(d); len(issues) != 0 {
		t.Fatal(issues)
	}
	if !CodeBearing(Document{CodeBearing: true}, "docs") || CodeBearing(d, "docs") || !CodeBearing(noRedDocument(), "code") {
		t.Fatal("class code-bearing")
	}
}

// FAE-WF-044: an overlay replaces the role's effort and named briefs only.
func TestEffectiveRoleOverlay(t *testing.T) {
	d := noRedDocument()
	r, ok := EffectiveRole(d, "docs", "writer")
	if !ok || r.Routes[0].Effort != "low" || r.Briefs["default"] != "docs brief" || d.Roles["writer"].Routes[0].Effort != "high" {
		t.Fatal(r)
	}
	if r, _ = EffectiveRole(d, "code", "writer"); r.Routes[0].Effort != "high" || r.Briefs["default"] != "brief" {
		t.Fatal("overlay leaked into another class", r)
	}
}

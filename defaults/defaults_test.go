package defaults

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func TestDefaultRoleSafetyPolicy(t *testing.T) {
	d := Workflow()
	if issues := workflow.Validate(d); len(issues) != 0 {
		t.Fatal(issues)
	}
	expected := map[string]int{"spec": 2, "spec-reviewer": 2, "planner": 2, "coder": 4, "reviewer-correctness": 4, "reviewer-quality": 4, "reviewer-security": 4, "reviewer-architecture": 4, "oracle": 1, "qa": 1}
	for name, cap := range expected {
		role, ok := d.Roles[name]
		if !ok || role.Cap != cap {
			t.Errorf("role %s cap %d want %d", name, role.Cap, cap)
		}
		providers := map[string]bool{}
		for _, route := range role.Routes {
			providers[route.Provider] = true
		}
		if len(role.Routes) < 4 || len(providers) < 2 {
			t.Errorf("role %s lacks route depth/diversity", name)
		}
	}
	for _, name := range []string{"spec", "spec-reviewer", "planner"} {
		profile := d.Roles[name].Profile
		if profile.Build || profile.Test || profile.Format {
			t.Fatalf("pre-code role %s granted heavy commands", name)
		}
	}
	if d.Limits.Attempts != 20 || d.Limits.TestRuns != 8 || d.Limits.ScopeWidens != 3 {
		t.Fatal("default caps drifted", d.Limits)
	}
}
func TestDefaultCopiesAndRefusalControls(t *testing.T) {
	first := Workflow()
	first.Roles["coder"] = workflow.Role{}
	if len(workflow.Validate(first)) == 0 {
		t.Fatal("invalid role not refused")
	}
	second := Workflow()
	if len(workflow.Validate(second)) != 0 {
		t.Fatal("mutation leaked into default")
	}
	for stage, steps := range HappyScript() {
		if len(steps) == 0 {
			t.Fatal("empty fixture", stage)
		}
		for _, step := range steps {
			if len(step.Reviews) == 0 && !json.Valid(step.Output) {
				t.Fatalf("invalid fixture %s", stage)
			}
		}
	}
}

func TestReconciliationRouteMustBeBounded(t *testing.T) {
	for _, mutation := range []string{"missing-target", "wrong-primitive", "missing-edge", "unbounded"} {
		t.Run(mutation, func(t *testing.T) {
			d := Workflow()
			merge := d.Stages["merge"]
			switch mutation {
			case "missing-target":
				merge.Reverify = "absent"
				d.Stages["merge"] = merge
			case "wrong-primitive":
				merge.Primitive = workflow.VcsCommit
				d.Stages["merge"] = merge
			case "missing-edge":
				edges := d.Edges[:0]
				for _, edge := range d.Edges {
					if edge.From != "merge" || edge.To != merge.Reverify {
						edges = append(edges, edge)
					}
				}
				d.Edges = edges
			case "unbounded":
				loop := d.Stages[merge.Reverify]
				loop.MaxIterations = 0
				d.Stages[merge.Reverify] = loop
			}
			if len(workflow.Validate(d)) == 0 {
				t.Fatal("unsafe reconciliation route accepted")
			}
		})
	}
}

// The shipped content must run live unmodified: every stage renders a template,
// role text matches FAE-WF-083, routes are real, the coder may edit and test
// runs prove a fresh build at the Appendix A weights.
func TestDefaultContentRunsLiveWithoutPatches(t *testing.T) {
	d := Workflow()
	for _, text := range RoleTexts() {
		roles := []string{text.Role}
		if text.Role == "reviewer" {
			roles = reviewers()
		}
		for _, name := range roles {
			got := d.Roles[name].Instructions
			for _, clause := range []string{text.Purpose, text.Produces, text.MustNot} {
				if !strings.Contains(got, clause) {
					t.Errorf("role %s instructions lack %q", name, clause)
				}
			}
		}
	}
	for id, s := range d.Stages {
		if s.Primitive != workflow.AgentTurn && s.Primitive != workflow.Review {
			continue
		}
		roles := []string{s.Role}
		if len(s.Reviewers) > 0 {
			roles = s.Reviewers
		}
		for _, role := range roles {
			// Mirrors the executor lookup: phase key, else default.
			template := d.Roles[role].Briefs[s.Phase]
			if template == "" {
				template = d.Roles[role].Briefs["default"]
			}
			if template == "" {
				t.Errorf("stage %s role %s renders an empty template", id, role)
			}
		}
	}
	for name, role := range d.Roles {
		if role.Routes[0] != (workflow.Binding{Provider: "claude", Model: "claude-opus-5-5", Effort: "medium"}) {
			t.Errorf("role %s does not prefer the operator's Claude binding", name)
		}
		for _, route := range role.Routes {
			if strings.HasPrefix(route.Provider, "scripted") {
				t.Errorf("role %s keeps rehearsal route %v", name, route)
			}
		}
		edits := false
		for _, grant := range role.Profile.Allowed {
			edits = edits || grant == "edit"
		}
		if edits != (name == "coder") {
			t.Errorf("role %s edit grant %v", name, edits)
		}
	}
	want := map[string]struct {
		weight int
		shape  string
	}{"run-red": {1, "go-unit"}, "run-tests": {1, "go-unit"}, "run-suite": {2, ""}, "qa": {2, ""}, "post-merge-qa": {2, ""}}
	for id, s := range d.Stages {
		if !s.TestRun {
			continue
		}
		w, ok := want[id]
		if !ok || s.Weight != w.weight || s.TestShape != w.shape {
			t.Errorf("test run %s weight %d shape %q", id, s.Weight, s.TestShape)
		}
		flags := map[string]bool{}
		for _, arg := range s.Command {
			flags[arg] = true
		}
		if !flags["-json"] || !flags["-count=1"] || !flags["-a"] || !flags["-x"] {
			t.Errorf("test run %s cannot prove a fresh build: %v", id, s.Command)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Error("missing test runs", want)
	}
}

// Each case weakens one test-first or positive-evidence floor of the default
// content and must be refused at the named path (FAE-WF-062, -073, -090).
func TestDefaultFloorMutationCorpus(t *testing.T) {
	cases := []struct {
		name, path, message string
		mutate              func(*workflow.Document)
	}{
		{"phase-less producer after compare", "stages.polish.phase", "declare", func(d *workflow.Document) {
			polish(d)
			redirect(d, "green-gate", "format", "polish")
			d.Edges = append(d.Edges, workflow.Edge{From: "polish", To: "format"})
		}},
		{"phase-less producer resets comparison", "stages.commit", "frozen RED comparison", func(d *workflow.Document) {
			polish(d)
			redirect(d, "green-gate", "format", "polish")
			d.Edges = append(d.Edges, workflow.Edge{From: "polish", To: "format"})
		}},
		{"RED never passes after implementation", "stages.commit", "passing run of the frozen RED test", func(d *workflow.Document) {
			s := d.Stages["green-gate"]
			x := and(eq("run-tests", "exit_code", 0), bin("ge", field("run-tests", "executed_count"), literal(1)), eq("compare-red", "unchanged", true))
			s.Expr = &x
			d.Stages["green-gate"] = s
		}},
		{"test-run gate without test_stage", "stages.qa-gate.expression", "executed_count", func(d *workflow.Document) {
			s := d.Stages["qa-gate"]
			x := eq("qa", "exit_code", 0)
			s.Expr = &x
			s.TestStage = ""
			d.Stages["qa-gate"] = s
		}},
		{"type-selected test run without count", "stages.post-merge-gate.expression", "executed_count", func(d *workflow.Document) {
			s := d.Stages["post-merge-gate"]
			x := and(eq("post-merge-qa", "exit_code", 0), bin("ge", field("post-merge-qa", "executed_count"), literal(1)), workflow.Expr{Op: "exists", Type: "EngineRun"})
			s.Expr = &x
			d.Stages["post-merge-gate"] = s
		}},
		{"RED run without declared test shape", "stages.freeze-red", "declared RED gate", func(d *workflow.Document) {
			s := d.Stages["run-red"]
			s.TestShape = ""
			d.Stages["run-red"] = s
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Workflow()
			tc.mutate(&d)
			issues := workflow.Validate(d)
			for _, i := range issues {
				if i.Path == tc.path && strings.Contains(i.Message, tc.message) {
					return
				}
			}
			t.Fatalf("want %s: %s, got %v", tc.path, tc.message, issues)
		})
	}
}
func polish(d *workflow.Document) {
	d.Stages["polish"] = workflow.Stage{Primitive: workflow.AgentTurn, Role: "coder", Output: "ImplementationReport", ChangeProducer: true}
	coder := d.Roles["coder"]
	coder.Briefs["default"] = "Polish the implementation."
	d.Roles["coder"] = coder
}
func redirect(d *workflow.Document, from, to, via string) {
	for i, e := range d.Edges {
		if e.From == from && e.To == to {
			d.Edges[i].To = via
		}
	}
}

// drive runs a scripted job until it stops, waits or finishes.
func drive(t *testing.T, script map[string][]engine.ScriptStep) engine.Job {
	t.Helper()
	j, _ := driveDispatches(t, Workflow(), script)
	return j
}
func driveDispatches(t *testing.T, d workflow.Document, script map[string][]engine.ScriptStep) (engine.Job, []engine.Dispatch) {
	t.Helper()
	dispatched := []engine.Dispatch{}
	for stage, steps := range script {
		for len(steps) < 10 {
			steps = append(steps, steps[len(steps)-1])
		}
		script[stage] = steps
	}
	j, err := engine.New("task", 1, d, "tree")
	if err != nil {
		t.Fatal(err)
	}
	executor := engine.NewScripted(script)
	for i := 0; i < 400; i++ {
		if j.State.Terminal() || j.State == domain.Escalated || j.Parked {
			return j, dispatched
		}
		next, d, _, err := engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		j = next
		if d == nil {
			continue
		}
		dispatched = append(dispatched, *d)
		r, err := executor.Execute(context.Background(), *d)
		if err != nil {
			t.Fatal(err)
		}
		if j, _, err = engine.Complete(j, d.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("unbounded")
	return j, dispatched
}
func routed(j engine.Job, from, to string) bool {
	for _, e := range j.History {
		if e.Kind == "transition" && string(e.From) == from && string(e.To) == to {
			return true
		}
	}
	return false
}

// FAE-WF-084 and Appendix C step 15: the oracle decides per finding; valid
// blocking findings route rework by target, an unknown target or an
// unapproved verdict escalates, and only zero valid blockers reach QA.
func TestOracleFindingRouting(t *testing.T) {
	finding := func(valid bool, severity, target string) map[string]any {
		return map[string]any{"finding": "fixture finding", "valid": valid, "severity": severity, "target": target, "rationale": "fixture"}
	}
	cases := []struct {
		name     string
		approved bool
		findings []map[string]any
		to       string
	}{
		{"pass", true, nil, "qa"},
		{"pass with non-blocking follow-up", true, []map[string]any{finding(true, "non-blocking", "Production")}, "qa"},
		{"invalid blocker ignored", true, []map[string]any{finding(false, "blocking", "Production")}, "qa"},
		{"production blocker", false, []map[string]any{finding(true, "blocking", "Production")}, "implement-loop"},
		{"approval cannot soften a blocker", true, []map[string]any{finding(true, "blocking", "Production")}, "implement-loop"},
		{"RED blocker first", false, []map[string]any{finding(true, "blocking", "Production"), finding(true, "blocking", "RedTest")}, "red-loop"},
		{"unknown target", false, []map[string]any{finding(true, "blocking", "RedTest"), finding(true, "blocking", "unknown")}, "verdict-stop"},
		{"rejected without a blocker", false, nil, "verdict-stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := HappyScript()
			findings := []map[string]any{}
			findings = append(findings, tc.findings...)
			script["oracle"] = []engine.ScriptStep{{Output: raw(map[string]any{"approved": tc.approved, "findings": findings, "rationale": "fixture"})}, script["oracle"][0]}
			j := drive(t, script)
			if !routed(j, "verdict", tc.to) {
				t.Fatalf("verdict did not route to %s: %s %s", tc.to, j.State, j.Reason)
			}
			if tc.to != "verdict-stop" && j.State != domain.Done {
				t.Fatal("rework did not converge", j.State, j.Reason)
			}
		})
	}
}

// Any blocking finding fails the review aggregate even with approval
// (Appendix C steps 3 and 13: any-blocking-fails).
func TestReviewAggregatesAnyBlockingFails(t *testing.T) {
	blocker := []map[string]any{{"finding": "fixture", "severity": "blocking", "target": "Production"}}
	script := HappyScript()
	bad := []json.RawMessage{}
	for _, dimension := range Dimensions {
		review := map[string]any{"approved": true, "dimension": dimension, "findings": []map[string]any{}}
		if dimension == "security" {
			review["findings"], review["sensitive_domains"], review["guidance"] = blocker, []string{}, []string{}
		}
		bad = append(bad, raw(review))
	}
	script["reviews"] = []engine.ScriptStep{{Reviews: bad}, script["reviews"][0]}
	j := drive(t, script)
	if !routed(j, "reviews-gate", "implement-loop") || j.State != domain.Done {
		t.Fatal("blocking review finding passed the aggregate", j.State, j.Reason)
	}
	script = HappyScript()
	spec := []map[string]any{{"finding": "ambiguous", "severity": "blocking"}}
	script["spec-review"] = []engine.ScriptStep{{Reviews: []json.RawMessage{raw(map[string]any{"approved": true, "findings": spec}), script["spec-review"][0].Reviews[1]}}, script["spec-review"][0]}
	j = drive(t, script)
	if !routed(j, "spec-gate", "spec-loop") || j.State != domain.Done {
		t.Fatal("blocking spec finding passed the aggregate", j.State, j.Reason)
	}
}

// Appendix C: a rework turn receives the failure that routed it back.
func TestReworkTurnsReceiveFailureEvidence(t *testing.T) {
	bound := func(dispatches []engine.Dispatch, stage string, visit int, stages ...string) bool {
		for _, d := range dispatches {
			if d.Stage != stage || d.Visit != visit {
				continue
			}
			have := map[string]bool{}
			for _, a := range d.Inputs {
				have[a.Stage] = true
			}
			for _, s := range stages {
				if !have[s] {
					return false
				}
			}
			return true
		}
		return false
	}
	script := HappyScript()
	rejected := raw(map[string]any{"approved": false, "findings": []map[string]any{{"finding": "untestable criterion", "severity": "blocking"}}})
	script["spec-review"] = []engine.ScriptStep{{Reviews: []json.RawMessage{rejected, script["spec-review"][0].Reviews[1]}}, script["spec-review"][0]}
	script["oracle"] = []engine.ScriptStep{{Output: raw(map[string]any{"approved": false, "findings": []map[string]any{{"finding": "RED asserts the wrong value", "valid": true, "severity": "blocking", "target": "RedTest", "rationale": "fixture"}}, "rationale": "fixture"})}, {Output: raw(map[string]any{"approved": false, "findings": []map[string]any{{"finding": "edge case", "valid": true, "severity": "blocking", "target": "Production", "rationale": "fixture"}}, "rationale": "fixture"})}, script["oracle"][0]}
	failing := map[string]any{"exit_code": 1, "executed_count": 3, "matched_count": 1, "expected_failure": false, "test_in_source": true, "red_passed": true, "fresh_build": true, "output_digest": "fixture"}
	script["qa"] = []engine.ScriptStep{{Output: raw(failing)}, script["qa"][0]}
	d := Workflow()
	d.Limits.Attempts, d.Limits.TestRuns = 100, 100
	j, dispatches := driveDispatches(t, d, script)
	if j.State != domain.Done {
		t.Fatal(j.State, j.Reason)
	}
	if !bound(dispatches, "spec", 2, "intake", "spec-review") {
		t.Error("spec rework lacks the spec review findings")
	}
	if !bound(dispatches, "test-first", 2, "plan", "oracle", "reviews", "run-red") {
		t.Error("RED rework lacks the oracle verdict and reviews")
	}
	if !bound(dispatches, "implement", 3, "freeze-red", "oracle", "reviews", "run-tests") {
		t.Error("production rework lacks the oracle verdict")
	}
	if !bound(dispatches, "implement", 4, "freeze-red", "qa") {
		t.Error("QA rework lacks the failing QA run")
	}
}

// simulationScript converts engine fixtures into offline simulator records.
func simulationScript(t *testing.T, task string, script map[string][]engine.ScriptStep) workflow.Script {
	t.Helper()
	d := Workflow()
	out := workflow.Script{}
	for stage, steps := range script {
		if p := d.Stages[stage].Primitive; p == workflow.FreezeArtifact || p == workflow.CompareFrozen {
			continue // the simulator computes identities like the scripted adapter
		}
		for visit, step := range steps {
			outputs := step.Reviews
			if len(outputs) == 0 {
				outputs = []json.RawMessage{step.Output}
			}
			records := []workflow.Evidence{}
			for i, o := range outputs {
				var fields map[string]any
				dec := json.NewDecoder(strings.NewReader(string(o)))
				dec.UseNumber()
				if err := dec.Decode(&fields); err != nil {
					t.Fatal(stage, err)
				}
				records = append(records, workflow.Evidence{ID: fmt.Sprintf("%s/%d/%d", stage, visit, i), TaskID: task, Type: d.Stages[stage].Output, Version: 1, Stage: stage, Provenance: true, Fields: fields})
			}
			out[stage] = append(out[stage], workflow.ScriptStep{Evidence: records})
		}
	}
	return out
}

// FAE-WF-091 acceptance: simulating the default workflow with "spec rejected
// once, oracle blocks once" reports the expected path, offline.
func TestSimulateDefaultScenario(t *testing.T) {
	script := HappyScript()
	rejected := raw(map[string]any{"approved": false, "findings": []map[string]any{{"finding": "untestable criterion", "severity": "blocking"}}})
	script["spec-review"] = []engine.ScriptStep{{Reviews: []json.RawMessage{rejected, script["spec-review"][0].Reviews[1]}}, script["spec-review"][0]}
	script["oracle"] = []engine.ScriptStep{{Output: raw(map[string]any{"approved": false, "findings": []map[string]any{{"finding": "edge case", "valid": true, "severity": "blocking", "target": "Production", "rationale": "fixture"}}, "rationale": "fixture"})}, script["oracle"][0]}
	for _, stage := range []string{"spec", "implement", "run-tests", "compare-red", "format", "reviews"} {
		script[stage] = append(script[stage], script[stage][0])
	}
	s, err := engine.Simulate(Workflow(), "task", simulationScript(t, "task", script), workflow.SimulationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	review := "spec-loop,spec,spec-review,spec-gate"
	build := "implement-loop,implement,green-route,run-tests,compare-red,green-gate,format,format-gate,reviews,reviews-gate,oracle,verdict"
	want := strings.Join([]string{"intake,intake-gate", review, review, "plan,red-route,red-loop,test-first,run-red,red-gate,freeze-red", build, build, "qa,qa-gate,merge,merge-route,post-merge-qa,post-merge-gate,commit,integrate,integration-gate,Done"}, ",")
	if got := strings.Join(s.Path, ","); s.State != domain.Done || got != want {
		t.Fatalf("path %s\nwant %s\nstate %s %s", got, want, s.State, s.Reason)
	}
	if len(s.Gates) != 13 || len(s.Transitions) != len(s.Path) {
		t.Fatal("gate or transition record incomplete", len(s.Gates), len(s.Transitions))
	}
	// The offline simulator and the durable engine take the same path.
	j := drive(t, script)
	durable := []string{}
	for _, e := range j.History {
		if e.Kind == "transition" {
			durable = append(durable, string(e.To))
		}
	}
	if strings.Join(durable, ",") != want {
		t.Fatalf("engine path %s", strings.Join(durable, ","))
	}
}

// FAE-WF-078: an implementation report may carry one typed request.
func TestImplementationReportRequestSchema(t *testing.T) {
	schema := Workflow().Artifacts["ImplementationReport"].Schema
	base := `"changed_files":["answer.go"],"hunks":["h"],"proof":"p","source":"s","test_id":"TestX"`
	for raw, ok := range map[string]bool{
		`{` + base + `}`: true,
		`{` + base + `,"request":{"kind":"scope-widen","files":["pkg/extra.go"],"reason":"helper"}}`: true,
		`{` + base + `,"request":{"kind":"red-rework","reason":"wrong assertion"}}`:                  true,
		`{` + base + `,"request":{"kind":"approve-self","reason":"x"}}`:                              false,
		`{` + base + `,"request":{"kind":"scope-widen","files":[]}}`:                                 false,
	} {
		if got := workflow.ValidateArtifact(schema, json.RawMessage(raw)) == nil; got != ok {
			t.Error(raw, got)
		}
	}
}

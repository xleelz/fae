package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/domain"
	"math/rand"
	"strings"
	"testing"
)

func minimal() Document {
	return Document{SchemaVersion: 1, ID: "demo", Start: "work", Roles: map[string]Role{}, Artifacts: map[string]ArtifactType{}, Stages: map[string]Stage{"work": {Primitive: Loop, MaxIterations: 2}}, Edges: []Edge{{"work", "Done"}}, Limits: Limits{20, 8, 3}}
}
func lit(v any) Expr                { return Expr{Op: "literal", Value: v} }
func bin(op string, a, b Expr) Expr { return Expr{Op: op, Args: []Expr{a, b}} }
func field(stage, path string) Expr { return Expr{Op: "field", Stage: stage, Path: []string{path}} }
func goodGate() Expr {
	return bin("and", bin("eq", field("test", "exit_code"), lit(0)), bin("ge", field("test", "executed_count"), lit(1)))
}
func protected() Document {
	d := minimal()
	d.Start = "change"
	d.Roles = map[string]Role{}
	for _, r := range []string{"coder", "reviewer"} {
		d.Roles[r] = Role{Instructions: "instruction", Briefs: map[string]string{"default": "brief"}, Routes: []Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"report"}}
	}
	d.Artifacts = map[string]ArtifactType{"report": {1, json.RawMessage(`{"type":"object"}`)}}
	x := goodGate()
	d.Stages = map[string]Stage{"change": {Primitive: AgentTurn, Role: "coder", Output: "report", ChangeProducer: true}, "test": {Primitive: HostCommand, Output: "report", Command: []string{"go", "test", "./..."}, Weight: 1, TimeoutSeconds: 30, TestRun: true}, "gate": {Primitive: Gate, Expr: &x, TestStage: "test", Failure: "stop"}, "review": {Primitive: Review, Role: "reviewer", Output: "report"}, "commit": {Primitive: VcsCommit, Output: "report", Protected: true}, "stop": {Primitive: Escalate}}
	d.Edges = []Edge{{"change", "test"}, {"test", "gate"}, {"gate", "review"}, {"gate", "stop"}, {"review", "commit"}, {"commit", "Done"}}
	return d
}
func TestKernelExhaustive(t *testing.T) {
	d := minimal()
	states := []domain.State{domain.Pending, domain.Blocked, domain.Parked, domain.Escalated, domain.Done, domain.Retired, domain.Cancelled, "work", "unknown"}
	for _, from := range states {
		for _, to := range states {
			want := false
			if from != to && from != domain.Parked && to != domain.Parked && from != "unknown" && to != "unknown" && !from.Terminal() {
				if from == domain.Escalated {
					want = to == domain.Retired || to == "work"
				} else {
					want = to == domain.Escalated || to == domain.Cancelled && (from == domain.Pending || from == domain.Blocked) || from == domain.Pending && to == "work" || from == "work" && (to == domain.Done || to == domain.Blocked)
				}
			}
			if got := CanTransition(d, from, to); got != want {
				t.Errorf("%s->%s got %v want %v", from, to, got, want)
			}
		}
	}
}

// FAE-WF-021/-008: every pair over a molecule with a pre-code stage, a gate
// failure route and a carry entry; the kernel edges are exactly the spec's.
func TestKernelExhaustiveFullGraph(t *testing.T) {
	d := protected()
	d.Start = "spec"
	d.Stages["spec"] = Stage{Primitive: AgentTurn, Role: "reviewer", Output: "report"}
	d.Edges = append(d.Edges, Edge{"spec", "change"})
	if issues := Validate(d); len(issues) > 0 {
		t.Fatal(issues)
	}
	edges := map[[2]domain.State]bool{}
	for _, e := range d.Edges {
		edges[[2]domain.State{domain.State(e.From), domain.State(e.To)}] = true
	}
	states := []domain.State{domain.Pending, domain.Blocked, domain.Parked, domain.Escalated, domain.Done, domain.Retired, domain.Cancelled, "unknown"}
	for id := range d.Stages {
		states = append(states, domain.State(id))
	}
	for _, from := range states {
		for _, to := range states {
			want := false
			stage := !from.Kernel() && from != "unknown"
			switch {
			case from.Terminal() || from == domain.Parked || to == domain.Parked || from == "unknown" || to == "unknown":
			case from == domain.Escalated:
				want = to == domain.Retired || to == "spec"
			case to == domain.Escalated:
				want = true
			case to == domain.Cancelled:
				want = from == domain.Pending || from == domain.Blocked
			case from == domain.Pending:
				// Release at the first stage, or at the carry entry (FAE-WF-086).
				want = to == "spec" || to == "change"
			case from == domain.Blocked:
				// Re-entry follows the declared remediation route (the gate's failure).
				want = to == "stop"
			case to == domain.Blocked:
				want = stage
			default:
				want = edges[[2]domain.State{from, to}]
			}
			if got := CanTransition(d, from, to); got != want {
				t.Errorf("%s->%s got %v want %v", from, to, got, want)
			}
		}
	}
	for _, terminal := range []domain.State{domain.Done, domain.Retired, domain.Cancelled} {
		for _, to := range states {
			if CanTransition(d, terminal, to) {
				t.Errorf("terminal %s has out-edge to %s", terminal, to)
			}
		}
	}
}
func TestStrictParse(t *testing.T) {
	d := minimal()
	b, _ := json.Marshal(d)
	if _, e := Parse(b); e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{strings.Replace(string(b), `"schema_version":1`, `"schema_version":1,"typo":true`, 1), strings.Replace(string(b), `"primitive":"Loop"`, `"primitive":"Loop","typo":true`, 1), strings.Replace(string(b), `"Loop"`, `"Invented"`, 1), strings.Replace(string(b), `"attempts":20`, `"attempts":"20"`, 1), `{"id":"a","id":"b"}`} {
		if _, e := Parse([]byte(raw)); e == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

// FAE-WF-090: one invalid workflow per rule, each refused at its document path.
func TestValidationMutationCorpus(t *testing.T) {
	if issues := Validate(protected()); len(issues) > 0 {
		t.Fatal(issues)
	}
	cases := []struct {
		name, path, message string
		mutate              func(*Document)
	}{
		{"bypass", "stages.commit", "without independent review and passing test gate", func(d *Document) { d.Edges = append(d.Edges, Edge{"change", "commit"}) }},
		{"self review", "stages.review.role", "reviewer must differ", func(d *Document) { s := d.Stages["review"]; s.Role = "coder"; d.Stages["review"] = s }},
		{"wrong gate", "stages.gate.expression", "test gate must assert", func(d *Document) { s := d.Stages["gate"]; s.TestStage = "change"; d.Stages["gate"] = s }},
		{"zero count", "stages.gate.expression", "executed_count", func(d *Document) {
			s := d.Stages["gate"]
			x := bin("eq", field("test", "exit_code"), lit(0))
			s.Expr = &x
			d.Stages["gate"] = s
		}},
		{"failure bypass", "stages.commit", "without independent review and passing test gate", func(d *Document) { s := d.Stages["gate"]; s.Failure = "review"; d.Stages["gate"] = s }},
		{"cycle", "stages.test", "cycle avoids bounded Loop", func(d *Document) { d.Edges = append(d.Edges, Edge{"review", "test"}) }},
		{"external pins", "stages.commit", "external effect requires", func(d *Document) { s := d.Stages["commit"]; s.Primitive = ExternalEffect; d.Stages["commit"] = s }},
		{"unreachable", "stages.island", "unreachable", func(d *Document) { d.Stages["island"] = Stage{Primitive: Escalate} }},
		{"kernel override", "edges[6]", "kernel edges cannot be overridden", func(d *Document) { d.Edges = append(d.Edges, Edge{"Done", "change"}) }},
		{"unknown input", "stages.review.inputs[0]", "unknown artifact", func(d *Document) {
			s := d.Stages["review"]
			s.Inputs = []Input{{Name: "bad", Type: "bad", Version: 1}}
			d.Stages["review"] = s
		}},
		{"dead end", "stages.commit", "no success route", func(d *Document) { d.Edges = d.Edges[:len(d.Edges)-1] }},
		{"no path to Done", "stages.review", "no path to Done", func(d *Document) {
			d.Edges = d.Edges[:len(d.Edges)-1]
			d.Edges = append(d.Edges, Edge{"commit", "stop"})
		}},
		{"ambiguous route", "stages.test", "ambiguous success route", func(d *Document) { d.Edges = append(d.Edges, Edge{"test", "review"}) }},
		{"ambiguous gate", "stages.gate", "ambiguous success route", func(d *Document) { d.Edges = append(d.Edges, Edge{"gate", "commit"}) }},
		{"missing output", "stages.test.output", "required", func(d *Document) { s := d.Stages["test"]; s.Output = ""; d.Stages["test"] = s }},
		{"unknown gate stage", "stages.gate.expression", "unknown stage tset", func(d *Document) {
			s := d.Stages["gate"]
			x := bin("and", goodGate(), bin("eq", field("tset", "exit_code"), lit(0)))
			s.Expr = &x
			d.Stages["gate"] = s
		}},
		{"unknown gate type", "stages.gate.expression", "unknown artifact type Rport", func(d *Document) {
			s := d.Stages["gate"]
			x := bin("and", goodGate(), Expr{Op: "exists", Type: "Rport"})
			s.Expr = &x
			d.Stages["gate"] = s
		}},
		{"unknown capability", "roles.coder.profile.allowed", "unknown capability", func(d *Document) {
			r := d.Roles["coder"]
			r.Profile.Allowed = []string{"handoff", "network"}
			d.Roles["coder"] = r
		}},
		{"unknown freeze subject", "stages.freeze.subject", "unknown stage", func(d *Document) {
			d.Stages["freeze"] = Stage{Primitive: FreezeArtifact, Output: "report", Subject: "chnage"}
			d.Edges[0] = Edge{"change", "freeze"}
			d.Edges = append(d.Edges, Edge{"freeze", "test"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := protected()
			tc.mutate(&d)
			issues := Validate(d)
			if !hasIssue(issues, tc.path, tc.message) {
				t.Fatalf("want %s: %s, got %v", tc.path, tc.message, issues)
			}
		})
	}
}

// FAE-WF-003/-004: structural errors name their document path and are listed
// with every semantic error in one refusal.
func TestParseNamesEveryErrorPath(t *testing.T) {
	d := protected()
	b, _ := json.Marshal(d)
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	doc["typo"] = true
	stages := doc["stages"].(map[string]any)
	stages["gate"].(map[string]any)["expression"].(map[string]any)["args"].([]any)[0].(map[string]any)["typo"] = 1
	stages["review"].(map[string]any)["role"] = "ghost"
	doc["limits"].(map[string]any)["attempts"] = "20"
	doc["roles"].(map[string]any)["coder"].(map[string]any)["routes"].([]any)[0].(map[string]any)["effort"] = 3
	raw, _ := json.Marshal(doc)
	_, err := Parse(raw)
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatal("not a validation error", err)
	}
	for _, want := range []struct{ path, message string }{
		{"typo", "unknown field"},
		{"stages.gate.expression.args[0].typo", "unknown field"},
		{"limits.attempts", "expected integer"},
		{"roles.coder.routes[0].effort", "expected string"},
		{"stages.review.role", "unknown role"},
	} {
		if !hasIssue(invalid.Issues, want.path, want.message) {
			t.Errorf("missing %s: %s in %v", want.path, want.message, invalid.Issues)
		}
	}
	if !strings.Contains(err.Error(), "stages.gate.expression.args[0].typo") {
		t.Fatal("error text omits the path", err)
	}
}

// Generated chains of bounded Loops, each followed by a Branch whose extra
// arms add random (back) edges: every cycle passes a Loop, terminals never
// escape, and removing any Loop cap is refused.
func TestGeneratedKernelAndBoundedCycles(t *testing.T) {
	rng := rand.New(rand.NewSource(60161))
	for iteration := 0; iteration < 10000; iteration++ {
		size := 1 + rng.Intn(31)
		d := minimal()
		d.Stages = map[string]Stage{}
		d.Edges = nil
		d.Start = "s0"
		for i := 0; i < size; i++ {
			loop, branch := fmt.Sprintf("s%d", i), fmt.Sprintf("b%d", i)
			to := fmt.Sprintf("s%d", i+1)
			if i == size-1 {
				to = "Done"
			}
			d.Stages[loop] = Stage{Primitive: Loop, MaxIterations: 1 + rng.Intn(20)}
			d.Stages[branch] = Stage{Primitive: Branch, Default: to}
			d.Edges = append(d.Edges, Edge{loop, branch}, Edge{branch, to})
		}
		if x := Validate(d); len(x) > 0 {
			t.Fatal(x)
		}
		for id := range d.Stages {
			for _, terminal := range []domain.State{domain.Done, domain.Retired, domain.Cancelled} {
				if CanTransition(d, terminal, domain.State(id)) {
					t.Fatal("terminal escape")
				}
			}
		}
		arm := func(from, to string) {
			b := d.Stages[from]
			b.Cases = append(b.Cases, Case{When: Expr{Op: "literal", Value: true}, Next: to})
			d.Stages[from] = b
			d.Edges = append(d.Edges, Edge{from, to})
		}
		for j := 0; j < rng.Intn(8); j++ {
			arm(fmt.Sprintf("b%d", rng.Intn(size)), fmt.Sprintf("s%d", rng.Intn(size)))
		}
		arm(fmt.Sprintf("b%d", size-1), "s0")
		if x := Validate(d); len(x) > 0 {
			t.Fatal(x)
		}
		id := fmt.Sprintf("s%d", rng.Intn(size))
		stage := d.Stages[id]
		stage.MaxIterations = 0
		d.Stages[id] = stage
		if !hasIssue(Validate(d), "stages."+id+".max_iterations", "finite positive cap") {
			t.Fatal("removed loop cap accepted")
		}
	}
}
func TestNoBypass(t *testing.T) {
	s := Stage{Inputs: []Input{{Name: "spec", Type: "Spec", Stage: "spec", Version: 2}}}
	a := Evidence{ID: "a", TaskID: "t", Type: "Spec", Stage: "spec", Version: 2, Provenance: true}
	if len(CheckInputs(s, "t", []Evidence{a})) > 0 {
		t.Fatal("valid rejected")
	}
	for i := 0; i < 6; i++ {
		b := a
		switch i {
		case 0:
			b.ID = ""
		case 1:
			b.TaskID = "other"
		case 2:
			b.Type = "Other"
		case 3:
			b.Stage = "other"
		case 4:
			b.Version = 1
		case 5:
			b.Provenance = false
		}
		if len(CheckInputs(s, "t", []Evidence{b})) != 1 {
			t.Fatal("accepted", i)
		}
	}
}

func TestProtectedRuntime(t *testing.T) {
	run := Evidence{TaskID: "task", Provenance: true, Tree: "tree", Fields: map[string]any{"exit_code": 0, "executed_count": 1}}
	review := Evidence{TaskID: "task", Provenance: true, Tree: "tree", Role: "reviewer", Session: "r", Fields: map[string]any{"approved": true}}
	change := Evidence{TaskID: "task", Provenance: true, Role: "coder", Session: "c"}
	if e := CheckProtectedEvidence("tree", []Evidence{run}, []Evidence{review}, []Evidence{change}); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 4; i++ {
		r := run
		v := review
		c := change
		switch i {
		case 0:
			r.Tree = "stale"
		case 1:
			v.Session = "c"
		case 2:
			v.Role = "coder"
		case 3:
			c.Provenance = false
		}
		if CheckProtectedEvidence("tree", []Evidence{r}, []Evidence{v}, []Evidence{c}) == nil {
			t.Fatal("accepted", i)
		}
	}
}
func TestFrozenIdentity(t *testing.T) {
	base := []byte("package x\nfunc TestA(t *T) { assert(1 == 2) }\n")
	reformat := []byte("package x\n// comment\nfunc TestA(t *T) {\n assert(1 == 2)\n}\nfunc TestB(t *T) {}\n")
	a, e := GoTestIdentity(base, []string{"TestA"})
	if e != nil {
		t.Fatal(e)
	}
	b, e := GoTestIdentity(reformat, []string{"TestA"})
	if e != nil || a != b {
		t.Fatal(a, b, e)
	}
	c, e := GoTestIdentity([]byte(strings.Replace(string(base), "1 == 2", "1 == 1", 1)), []string{"TestA"})
	if e != nil || a == c {
		t.Fatal("change not detected")
	}
	for _, names := range [][]string{nil, {"missing"}, {"TestA", "TestA"}} {
		if _, e := GoTestIdentity(base, names); e == nil {
			t.Fatal("accepted", names)
		}
	}
}

// FAE-WF-041, FAE-WF-030: route bindings parse ordered provider, model,
// reasoning effort and auto_approval (default false), refuse empty routes or
// invalid fields, and human gates refuse auto_approval or non-stay-waiting
// timeout configuration.
func TestRouteBindingsAndAutoApprovalParse(t *testing.T) {
	d := protected()
	coder := d.Roles["coder"]
	coder.Routes = []Binding{
		{Provider: "codex", Model: "gpt-6-astra"},
		{Provider: "codex", Model: "gpt-6-astra-mini", Effort: "minimal", AutoApproval: true},
		{Provider: "claude", Model: "claude-sonnet-5-5", Effort: "low"},
		{Provider: "claude", Model: "claude-opus-5-5", Effort: "medium", AutoApproval: false},
		{Provider: "claude", Model: "claude-opus-5-5", Effort: "high", AutoApproval: true},
		{Provider: "codex", Model: "gpt-6-astra", Effort: "xhigh"},
	}
	d.Roles["coder"] = coder
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.Roles["coder"].Routes
	if len(got) != len(coder.Routes) {
		t.Fatalf("routes len=%d want=%d", len(got), len(coder.Routes))
	}
	for i, want := range coder.Routes {
		if got[i] != want {
			t.Fatalf("route[%d]=%+v want %+v", i, got[i], want)
		}
	}

	// Empty route list, missing provider/model, invalid effort, and mistyped auto_approval are refused.
	emptyRoutes := protected()
	r := emptyRoutes.Roles["coder"]
	r.Routes = []Binding{}
	emptyRoutes.Roles["coder"] = r
	if rawEmpty, _ := json.Marshal(emptyRoutes); true {
		_, err := Parse(rawEmpty)
		var invalid *ValidationError
		if !errors.As(err, &invalid) || !hasIssue(invalid.Issues, "roles.coder.routes", "required") {
			t.Fatalf("empty routes not refused: %v", err)
		}
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["roles"].(map[string]any)["coder"].(map[string]any)["routes"].([]any)[0].(map[string]any)["auto_approval"] = "yes"
	doc["stages"].(map[string]any)["gate"].(map[string]any)["auto_approval"] = true
	rawBad, _ := json.Marshal(doc)
	_, err = Parse(rawBad)
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	for _, want := range []struct{ path, message string }{
		{"roles.coder.routes[0].auto_approval", "expected boolean"},
		{"stages.gate.auto_approval", "unknown field"},
	} {
		if !hasIssue(invalid.Issues, want.path, want.message) {
			t.Fatalf("missing %s: %s in %v", want.path, want.message, invalid.Issues)
		}
	}
}

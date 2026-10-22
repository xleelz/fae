package workflow

import (
	"encoding/json"
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
		d.Roles[r] = Role{Instructions: "instruction", Briefs: map[string]string{"main": "brief"}, Routes: []Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"report"}}
	}
	d.Artifacts = map[string]ArtifactType{"report": {1, json.RawMessage(`{"type":"object"}`)}}
	x := goodGate()
	d.Stages = map[string]Stage{"change": {Primitive: AgentTurn, Role: "coder", Output: "report", ChangeProducer: true}, "test": {Primitive: HostCommand, Command: []string{"go", "test", "./..."}, Weight: 1, TimeoutSeconds: 30, TestRun: true}, "gate": {Primitive: Gate, Expr: &x, TestStage: "test", Failure: "stop"}, "review": {Primitive: Review, Role: "reviewer", Output: "report"}, "commit": {Primitive: VcsCommit, Protected: true}, "stop": {Primitive: Escalate}}
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
					want = to == domain.Escalated || to == domain.Cancelled && (from == domain.Pending || from == domain.Blocked) || from == domain.Pending && to == "work" || from == "work" && to == domain.Done
				}
			}
			if got := CanTransition(d, from, to); got != want {
				t.Errorf("%s->%s got %v want %v", from, to, got, want)
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
func TestValidationMutationCorpus(t *testing.T) {
	if issues := Validate(protected()); len(issues) > 0 {
		t.Fatal(issues)
	}
	cases := []struct {
		name   string
		mutate func(*Document)
	}{
		{"bypass", func(d *Document) { d.Edges = append(d.Edges, Edge{"change", "commit"}) }},
		{"self review", func(d *Document) { s := d.Stages["review"]; s.Role = "coder"; d.Stages["review"] = s }},
		{"wrong gate", func(d *Document) { s := d.Stages["gate"]; s.TestStage = "change"; d.Stages["gate"] = s }},
		{"zero count", func(d *Document) {
			s := d.Stages["gate"]
			x := bin("eq", field("test", "exit_code"), lit(0))
			s.Expr = &x
			d.Stages["gate"] = s
		}},
		{"failure bypass", func(d *Document) { s := d.Stages["gate"]; s.Failure = "review"; d.Stages["gate"] = s }},
		{"cycle", func(d *Document) { d.Edges = append(d.Edges, Edge{"review", "test"}) }},
		{"external pins", func(d *Document) { s := d.Stages["commit"]; s.Primitive = ExternalEffect; d.Stages["commit"] = s }},
		{"unreachable", func(d *Document) { d.Stages["island"] = Stage{Primitive: Escalate} }},
		{"kernel override", func(d *Document) { d.Edges = append(d.Edges, Edge{"Done", "change"}) }},
		{"unknown input", func(d *Document) {
			s := d.Stages["review"]
			s.Inputs = []Input{{Name: "bad", Type: "bad", Version: 1}}
			d.Stages["review"] = s
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := protected()
			tc.mutate(&d)
			if len(Validate(d)) == 0 {
				t.Fatal("accepted unsafe graph")
			}
		})
	}
}
func TestGeneratedKernelAndBoundedCycles(t *testing.T) {
	rng := rand.New(rand.NewSource(60161))
	for iteration := 0; iteration < 10000; iteration++ {
		size := 1 + rng.Intn(31)
		d := minimal()
		d.Stages = map[string]Stage{}
		d.Edges = nil
		d.Start = "s0"
		for i := 0; i < size; i++ {
			id := fmt.Sprintf("s%d", i)
			d.Stages[id] = Stage{Primitive: Loop, MaxIterations: 1 + rng.Intn(20)}
			to := fmt.Sprintf("s%d", i+1)
			if i == size-1 {
				to = "Done"
			}
			d.Edges = append(d.Edges, Edge{id, to})
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
		for j := 0; j < rng.Intn(8); j++ {
			d.Edges = append(d.Edges, Edge{fmt.Sprintf("s%d", rng.Intn(size)), fmt.Sprintf("s%d", rng.Intn(size))})
		}
		d.Edges = append(d.Edges, Edge{fmt.Sprintf("s%d", size-1), "s0"})
		if x := Validate(d); len(x) > 0 {
			t.Fatal(x)
		}
		id := fmt.Sprintf("s%d", rng.Intn(size))
		stage := d.Stages[id]
		stage.MaxIterations = 0
		d.Stages[id] = stage
		if len(Validate(d)) == 0 {
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
func TestSimulate(t *testing.T) {
	d := minimal()
	s, e := Simulate(d, "t", Script{"work": {{}}})
	if e != nil || s.State != domain.Done || len(s.Transitions) != 2 {
		t.Fatal(s, e)
	}
	d.Edges = []Edge{{"work", "work"}, {"work", "Done"}}
	s, e = Simulate(d, "t", Script{"work": {{Next: "work"}, {Next: "work"}}})
	if e != nil || s.State != domain.Escalated || s.Reason != "LoopCap" {
		t.Fatal(s, e)
	}
	d = protected()
	evidence := Evidence{ID: "run", TaskID: "t", Stage: "test", Type: "report", Version: 1, Provenance: true, Fields: map[string]any{"exit_code": 0, "executed_count": 1}}
	script := Script{"change": {{}}, "test": {{Evidence: []Evidence{evidence}}}, "review": {{}}, "commit": {{}}}
	s, e = Simulate(d, "t", script)
	if e == nil || !strings.Contains(e.Error(), "runtime evidence") {
		t.Fatal(s, e)
	}
	evidence.Fields["executed_count"] = 0
	script["test"] = []ScriptStep{{Evidence: []Evidence{evidence}}}
	s, e = Simulate(d, "t", script)
	if e != nil || s.State != domain.Escalated {
		t.Fatal(s, e)
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

package workflow

import (
	"strings"
	"testing"
)

// hasIssue reports whether issues name path with a message containing text.
func hasIssue(issues []Issue, path, text string) bool {
	for _, i := range issues {
		if i.Path == path && strings.Contains(i.Message, text) {
			return true
		}
	}
	return false
}

func TestBriefTemplatesResolvePerPhase(t *testing.T) {
	d := protected()
	if issues := Validate(d); len(issues) != 0 {
		t.Fatal(issues)
	}
	coder := d.Roles["coder"]
	coder.Briefs = map[string]string{"main": "unused key"}
	d.Roles["coder"] = coder
	issues := Validate(d)
	if !hasIssue(issues, "roles.coder.briefs.main", "default or a declared phase") || !hasIssue(issues, "stages.change.role", "no brief template") {
		t.Fatal("unselectable brief key accepted", issues)
	}
	change := d.Stages["change"]
	change.Phase = "implement"
	d.Stages["change"] = change
	coder.Briefs = map[string]string{"test-first": "only the RED phase"}
	d.Roles["coder"] = coder
	issues = Validate(d)
	if !hasIssue(issues, "roles.coder.briefs.test-first", "default or a declared phase") || !hasIssue(issues, "stages.change.role", "no brief template for phase implement") {
		t.Fatal("phase without template accepted", issues)
	}
	coder.Briefs = map[string]string{"implement": "make the RED pass"}
	d.Roles["coder"] = coder
	if issues = Validate(d); len(issues) != 0 {
		t.Fatal("phase template refused", issues)
	}
	coder.Briefs = map[string]string{"implement": " "}
	d.Roles["coder"] = coder
	if issues = Validate(d); !hasIssue(issues, "roles.coder.briefs.implement", "empty") {
		t.Fatal("blank template accepted", issues)
	}
}

func TestDeclaredTestShape(t *testing.T) {
	d := protected()
	test := d.Stages["test"]
	test.TestShape = "go-unit"
	d.Stages["test"] = test
	if issues := Validate(d); len(issues) != 0 {
		t.Fatal(issues)
	}
	for shape, command := range map[string][]string{"go-integration": {"go", "test", "-json", "./integration"}, "script": {"./scripts/red.sh"}, "runner": {"node", "--test"}} {
		d := protected()
		s := d.Stages["test"]
		s.TestShape, s.Command = shape, command
		d.Stages["test"] = s
		if issues := Validate(d); len(issues) != 0 {
			t.Fatal(shape, issues)
		}
	}
	for name, mutate := range map[string]func(*Stage){
		"unknown form":       func(s *Stage) { s.TestShape = "rust-integration" },
		"not a test":         func(s *Stage) { s.TestRun = false },
		"unit not go test":   func(s *Stage) { s.Command = []string{"pytest", "-q"} },
		"script not local":   func(s *Stage) { s.TestShape = "script"; s.Command = []string{"/usr/bin/red"} },
		"script escapes":     func(s *Stage) { s.TestShape = "script"; s.Command = []string{"./../red.sh"} },
		"runner is go":       func(s *Stage) { s.TestShape = "runner"; s.Command = []string{"go", "test", "./..."} },
		"runner is a path":   func(s *Stage) { s.TestShape = "runner"; s.Command = []string{"./bin/runner"} },
		"integration not go": func(s *Stage) { s.TestShape = "go-integration"; s.Command = []string{"cargo", "test"} },
	} {
		t.Run(name, func(t *testing.T) {
			d := protected()
			s := d.Stages["test"]
			s.TestShape = "go-unit"
			mutate(&s)
			d.Stages["test"] = s
			if !hasIssue(Validate(d), "stages.test.test_shape", "test-run HostCommand") {
				t.Fatal("invalid test shape accepted")
			}
		})
	}
}

// FAE-WF-076: a formatter pass, comments or an added test leave the frozen RED
// intact; changing anything the RED relies on does not.
func TestFrozenRedFileCompatibility(t *testing.T) {
	frozen := "package sample\n\nimport \"testing\"\n\nconst want = 42\n\nfunc check() bool { return answer() == want }\n\nfunc TestBehavior(t *testing.T) {\n\tif !check() {\n\t\tt.Fatal(\"got 0 want 42\")\n\t}\n}\n"
	if err := CheckFrozenFile([]byte(frozen), []byte(frozen), []string{"TestBehavior"}); err != nil {
		t.Fatal(err)
	}
	accepted := map[string]string{
		"formatter pass": "package sample\nimport \"testing\"\nconst want = 42\nfunc check() bool {\n\treturn answer() == want\n}\nfunc TestBehavior(t *testing.T) { if !check() { t.Fatal(\"got 0 want 42\") } }\n",
		"comments":       "// Package sample.\n" + frozen + "// trailing note\n",
		"added test":     frozen + "\nfunc TestEdge(t *testing.T) {\n\tif answer() < 0 {\n\t\tt.Fatal(\"negative\")\n\t}\n}\n",
		"added import":   strings.Replace(frozen, "import \"testing\"", "import (\n\t\"strings\"\n\t\"testing\"\n)", 1) + "\nfunc TestName(t *testing.T) { _ = strings.ToUpper(\"x\") }\n",
	}
	for name, current := range accepted {
		if err := CheckFrozenFile([]byte(frozen), []byte(current), []string{"TestBehavior"}); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	refused := map[string]string{
		"helper changed":    strings.Replace(frozen, "answer() == want", "true", 1),
		"constant changed":  strings.Replace(frozen, "want = 42", "want = 0", 1),
		"assertion changed": strings.Replace(frozen, "!check()", "check()", 1),
		"helper removed":    strings.Replace(frozen, "func check() bool { return answer() == want }\n", "", 1),
		"init added":        frozen + "\nfunc init() { println() }\n",
		"TestMain added":    frozen + "\nfunc TestMain(m *testing.M) { m.Run() }\n",
		"blank import":      strings.Replace(frozen, "import \"testing\"", "import (\n\t_ \"os\"\n\t\"testing\"\n)", 1),
		"import removed":    strings.Replace(strings.Replace(frozen, "import \"testing\"\n", "", 1), "t *testing.T", "t interface{ Fatal(...any) }", 1),
		"package changed":   strings.Replace(frozen, "package sample", "package other", 1),
		"unparsable":        frozen + "\nfunc {",
	}
	for name, current := range refused {
		if err := CheckFrozenFile([]byte(frozen), []byte(current), []string{"TestBehavior"}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := CheckFrozenFile([]byte(frozen), []byte(frozen), nil); err == nil {
		t.Error("missing declared test accepted")
	}
}

// Element counts let gates and branches judge typed findings inside one
// artifact (FAE-WF-060 counts over collections with a filter).
func TestElementCount(t *testing.T) {
	item := func(path string) Expr { return Expr{Op: "item", Path: []string{path}} }
	blocking := bin("and", bin("eq", item("valid"), lit(true)), bin("eq", item("blocking"), lit(true)))
	findings := func(filter ...Expr) Expr {
		return Expr{Op: "count", Stage: "oracle", Path: []string{"findings"}, Args: filter}
	}
	verdict := Evidence{ID: "v", Stage: "oracle", Fields: map[string]any{"findings": []any{
		map[string]any{"valid": true, "blocking": true, "target": "RedTest"},
		map[string]any{"valid": false, "blocking": true, "target": "Production"},
		map[string]any{"valid": true, "blocking": false, "target": "Production"},
	}}}
	older := Evidence{ID: "old", Stage: "oracle", Fields: map[string]any{"findings": []any{map[string]any{"valid": true, "blocking": true, "target": "Production"}}}}
	cases := []struct {
		x    Expr
		want Truth
	}{
		{bin("eq", findings(), lit(3)), True},
		{bin("eq", findings(blocking), lit(1)), True},
		{bin("eq", findings(bin("and", blocking, bin("eq", item("target"), lit("RedTest")))), lit(1)), True},
		{bin("eq", findings(bin("and", blocking, bin("eq", item("target"), lit("Production")))), lit(0)), True},
		{bin("eq", findings(bin("eq", item("severity"), lit("blocking"))), lit(0)), Unknown},
		{bin("eq", Expr{Op: "count", Stage: "oracle", Path: []string{"rationale"}}, lit(0)), Unknown},
		{bin("eq", Expr{Op: "count", Stage: "absent", Path: []string{"findings"}}, lit(0)), Unknown},
		{bin("eq", Expr{Op: "count", Stage: "oracle", Path: []string{"findings"}, All: true, Args: []Expr{blocking}}, lit(2)), True},
		// Nested: count evidence records whose own findings hold no valid blocker.
		{bin("eq", Expr{Op: "count", Stage: "oracle", Args: []Expr{bin("eq", findings(blocking), lit(0))}}, lit(0)), True},
		{bin("eq", item("valid"), lit(true)), Unknown},
	}
	verdict.Fields["rationale"] = "text"
	for i, c := range cases {
		if got := Eval(c.x, []Evidence{older, verdict}).Value; got != c.want {
			t.Errorf("case %d: got %s want %s", i, got, c.want)
		}
	}
	if err := ValidateExpr(bin("eq", findings(blocking), lit(0))); err != nil {
		t.Fatal(err)
	}
	for name, x := range map[string]Expr{
		"item outside element count":     bin("eq", item("valid"), lit(true)),
		"item in evidence count":         Expr{Op: "count", Stage: "oracle", Args: []Expr{bin("eq", item("valid"), lit(true))}},
		"item with selector":             Expr{Op: "count", Stage: "oracle", Path: []string{"findings"}, Args: []Expr{bin("eq", Expr{Op: "item", Stage: "oracle"}, lit(true))}},
		"element count without selector": Expr{Op: "count", Path: []string{"findings"}},
	} {
		if ValidateExpr(x) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

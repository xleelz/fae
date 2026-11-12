package workflow

import (
	"encoding/json"
	"slices"
	"testing"
)

func templated() Document {
	d := protected()
	d.Artifacts["Spec"] = ArtifactType{1, json.RawMessage(`{"type":"object","properties":{"acceptance":{"type":"string"},"test":{"type":"string"},"count":{"type":"integer"},"tags":{"type":"array"}}}`)}
	d.Roles["coder"] = Role{Instructions: "instruction", Briefs: map[string]string{"default": "brief"}, Routes: []Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"report", "Spec"}}
	s := d.Stages["test"]
	s.Command = []string{"runner", "--name", "{{test}}", "--", "{{acceptance}}"}
	s.Inputs = []Input{{Name: "spec", Type: "Spec", Stage: "change", Version: 1}}
	s.Bindings = map[string]ArgBinding{"test": {Type: "test-id", Input: "spec", Field: "test"}, "acceptance": {Type: "text", Input: "spec", Field: "acceptance"}}
	d.Stages["test"] = s
	c := d.Stages["change"]
	c.Output = "Spec"
	d.Stages["change"] = c
	return d
}

// FAE-WF-035: acceptance text full of shell metacharacters is bound as one
// literal argument; nothing is split, expanded or interpolated.
func TestBindCommandKeepsMetacharactersLiteral(t *testing.T) {
	d := templated()
	if issues := Validate(d); len(issues) != 0 {
		t.Fatal(issues)
	}
	hostile := "x; rm -rf / && $(id) `uname` | cat > pwned * ? ~ 'q' \"d\" \\ {{test}}"
	in := []Evidence{{Type: "Spec", Version: 1, Stage: "change", Fields: map[string]any{"acceptance": hostile, "test": "pkg::case_one"}}}
	argv, err := BindCommand(d.Stages["test"], in)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"runner", "--name", "pkg::case_one", "--", hostile}; !slices.Equal(argv, want) {
		t.Fatalf("bound %q", argv)
	}
	for name, fields := range map[string]map[string]any{
		"option injection": {"acceptance": "-rf", "test": "ok"},
		"newline":          {"acceptance": "a\nb", "test": "ok"},
		"spaced test id":   {"acceptance": "a", "test": "two words"},
		"missing field":    {"acceptance": "a"},
		"non-scalar":       {"acceptance": []any{"a"}, "test": "ok"},
	} {
		if _, err := BindCommand(d.Stages["test"], []Evidence{{Type: "Spec", Version: 1, Stage: "change", Fields: fields}}); err == nil {
			t.Error(name, "bound")
		}
	}
	if _, err := BindCommand(d.Stages["test"], append(in, in[0])); err == nil {
		t.Fatal("ambiguous binding accepted")
	}
	if _, err := BindCommand(d.Stages["test"], nil); err == nil {
		t.Fatal("missing artifact bound")
	}
}

func TestCheckArgTypes(t *testing.T) {
	for _, c := range []struct {
		typ, value string
		ok         bool
	}{
		{"go-test-name", "TestBehavior_2", true}, {"go-test-name", "TestX$", false}, {"go-test-name", "Behavior", false},
		{"test-id", "tests/test_a.py::test_b[1]", true}, {"test-id", "a b", false},
		{"relative-path", "pkg/a_test.go", true}, {"relative-path", "../x", false}, {"relative-path", "/etc/passwd", false}, {"relative-path", ".git/config", false}, {"relative-path", "a/../b", false},
		{"go-package", "./internal/it", true}, {"go-package", ".", true}, {"go-package", "./...", false}, {"go-package", "internal/it", false}, {"go-package", "./a/../..", false},
		{"text", "a; b | c", true}, {"text", "-v", false}, {"text", "a\x00", false}, {"text", "", false},
		{"integer", "42", true}, {"integer", "4.2", false}, {"unknown", "x", false},
	} {
		if got := CheckArg(c.typ, c.value) == nil; got != c.ok {
			t.Errorf("%s %q: got %v", c.typ, c.value, got)
		}
	}
}

func TestBindingValidationCorpus(t *testing.T) {
	cases := map[string]struct {
		path   string
		mutate func(*Stage)
	}{
		"partial":        {"stages.test.command[2]", func(s *Stage) { s.Command[2] = "-run={{test}}" }},
		"undeclared":     {"stages.test.command[2]", func(s *Stage) { s.Command[2] = "{{other}}" }},
		"program":        {"stages.test.command[0]", func(s *Stage) { s.Command[0] = "{{test}}" }},
		"unused":         {"stages.test.bindings.extra", func(s *Stage) { s.Bindings["extra"] = ArgBinding{Type: "text", Input: "spec", Field: "acceptance"} }},
		"unknown type":   {"stages.test.bindings.test.type", func(s *Stage) { s.Bindings["test"] = ArgBinding{Type: "shell", Input: "spec", Field: "test"} }},
		"unknown input":  {"stages.test.bindings.test.input", func(s *Stage) { s.Bindings["test"] = ArgBinding{Type: "text", Input: "nope", Field: "test"} }},
		"optional input": {"stages.test.bindings.test.input", func(s *Stage) { s.Inputs[0].Optional = true }},
		"unknown field":  {"stages.test.bindings.test.field", func(s *Stage) { s.Bindings["test"] = ArgBinding{Type: "text", Input: "spec", Field: "absent"} }},
		"array field":    {"stages.test.bindings.test.field", func(s *Stage) { s.Bindings["test"] = ArgBinding{Type: "text", Input: "spec", Field: "tags"} }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := templated()
			s := d.Stages["test"]
			s.Command = append([]string(nil), s.Command...)
			s.Inputs = append([]Input(nil), s.Inputs...)
			bindings := map[string]ArgBinding{}
			for k, v := range s.Bindings {
				bindings[k] = v
			}
			s.Bindings = bindings
			c.mutate(&s)
			d.Stages["test"] = s
			if issues := Validate(d); !hasIssue(issues, c.path, "") {
				t.Fatal("accepted or wrong path", issues)
			}
		})
	}
}

func TestShapeMatching(t *testing.T) {
	d := templated()
	shape := []string{"runner", "--name", "{{test-id}}", "--", "{{text}}"}
	if !MatchTemplate(shape, d.Stages["test"]) || MatchTemplate([]string{"runner", "--name", "{{text}}", "--", "{{text}}"}, d.Stages["test"]) || MatchTemplate(shape[:4], d.Stages["test"]) {
		t.Fatal("template matching")
	}
	if !MatchShape(shape, []string{"runner", "--name", "a::b", "--", "x; y"}) || MatchShape(shape, []string{"runner", "--name", "a b", "--", "x"}) || MatchShape(shape, []string{"runner", "--other", "a", "--", "x"}) || MatchShape([]string{"{{text}}"}, []string{"sh"}) {
		t.Fatal("bound matching")
	}
}

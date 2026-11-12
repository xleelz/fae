package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestArtifactsOnlyAndWriteScopeValidation(t *testing.T) {
	d := minimal()
	d.Produces = "artifacts"
	d.CodeBearing = true
	d.Start = "write"
	d.Roles = map[string]Role{"writer": {Instructions: "write a report", Briefs: map[string]string{"default": "write"}, Routes: []Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"report"}, Session: "agentic", Writes: []string{"reports/**"}, Profile: Profile{Allowed: []string{"write", "handoff"}}}}
	d.Artifacts = map[string]ArtifactType{"report": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}}
	d.Stages = map[string]Stage{"write": {Primitive: AgentTurn, Role: "writer", Output: "report"}}
	d.Edges = []Edge{{"write", "Done"}}
	if issues := Validate(d); len(issues) > 0 {
		t.Fatal(issues)
	}
	d.Produces = ""
	r := d.Roles["writer"]
	r.Writes = []string{"judge/**"}
	d.Roles["writer"] = r
	d.CodeBearing = false
	s := d.Stages["write"]
	s.Writes = []string{"fragments/**"}
	d.Stages["write"] = s
	if issues := Validate(d); !strings.Contains(issuesText(issues), "widens role") {
		t.Fatalf("widening accepted: %v", issues)
	}
	s.Writes = []string{"judge/reports/**"}
	s.Scratch = []string{"judge/**"}
	d.Stages["write"] = s
	if issues := Validate(d); !strings.Contains(issuesText(issues), "scratch overlaps") {
		t.Fatalf("overlap accepted: %v", issues)
	}
}
func issuesText(v []Issue) string { b, _ := json.Marshal(v); return string(b) }
func TestLiteralShellAllowAndTargetTestDenials(t *testing.T) {
	p := Profile{ShellAllow: [][]string{{"scripts/mcheck", "*"}}}
	if argv, err := CheckShellCommand(p, `scripts/mcheck 'a b.yaml'`, nil); err != nil || len(argv) != 2 || argv[1] != "a b.yaml" {
		t.Fatalf("literal allow %v %v", argv, err)
	}
	for _, command := range []string{"cd x && scripts/mcheck a", "FOO=1 scripts/mcheck a", "scripts/mcheck a | cat", `sh -c "scripts/mcheck a"`, "scripts/mcheck $(echo a)", "scripts/mcheck a; true", "scripts/mcheck a\ntrue", "go test ./..."} {
		if _, err := CheckShellCommand(p, command, nil); err == nil {
			t.Errorf("accepted %q", command)
		}
	}
	p = Profile{DenyTestRuns: true}
	for _, command := range []string{"pytest -q", "go test ./...", "npm test", "scripts/custom-test arg"} {
		if _, err := CheckShellCommand(p, command, [][]string{{"scripts/custom-test"}}); err == nil {
			t.Errorf("test runner accepted %q", command)
		}
	}
	if !ShellAllowNarrows([][]string{{"scripts/mcheck", "a"}}, [][]string{{"scripts/mcheck", "*"}}) || ShellAllowNarrows([][]string{{"go", "test", "*"}}, [][]string{{"scripts/mcheck", "*"}}) {
		t.Fatal("shell narrowing incorrect")
	}
}
func TestEmbeddedRuntimeArgumentsRemainLiteral(t *testing.T) {
	s := Stage{Command: []string{"checker", "--area={{input.area}}", "{{output_dir}}"}}
	if !MatchTemplate(s.Command, s) {
		t.Fatal("runtime template refused")
	}
	argv, err := BindCommandValues(s, nil, map[string]any{"input.area": "a; touch /tmp/nope", "output_dir": "/isolated/out"})
	if !MatchShape(s.Command, argv) || MatchShape(s.Command, []string{"checker", "--other=bad", "/isolated/out"}) {
		t.Fatal("runtime shape boundary")
	}
	if err != nil || len(argv) != 3 || argv[1] != "--area=a; touch /tmp/nope" {
		t.Fatalf("literal binding %v %v", argv, err)
	}
}

func TestRuntimeArgumentSubstitutionNeverReparsesBoundData(t *testing.T) {
	stage := Stage{Command: []string{"checker", "{{input.a}}/{{input.b}}"}}
	argv, err := BindCommandValues(stage, nil, map[string]any{"input.a": "{{input.b}}", "input.b": "value"})
	if err != nil || len(argv) != 2 || argv[1] != "{{input.b}}/value" {
		t.Fatalf("bound data reparsed: %v %v", argv, err)
	}
}

func TestItemBriefUsesNearestDeclaredBranchSchema(t *testing.T) {
	d := Document{Signature: &Signature{Inputs: map[string]json.RawMessage{"areas": json.RawMessage(`{"type":"array","items":{"type":"object","required":["name","meta"],"properties":{"name":{"type":"string"},"meta":{"type":"object","required":["rank"],"properties":{"rank":{"type":"integer"}}}}}}`)}}, Stages: map[string]Stage{
		"fan":    {Primitive: FanOut, BranchesFrom: &BranchesFromSpec{Input: "areas", Min: 1, Max: 3}, BranchTemplate: &FanBranch{ID: "area", Start: "inside"}, Join: "join"},
		"inside": {Primitive: AgentTurn, Brief: "area {{item.name}} at {{item.meta.rank}}"},
		"join":   {Primitive: FanIn}, "outside": {Primitive: AgentTurn},
	}, Edges: []Edge{{From: "fan", To: "inside"}, {From: "inside", To: "join"}, {From: "join", To: "outside"}, {From: "outside", To: "Done"}}}
	check := func() []string {
		var issues []string
		validateBriefTemplates(d, func(path, message string) { issues = append(issues, path+": "+message) })
		return issues
	}
	if issues := check(); len(issues) > 0 {
		t.Fatal(issues)
	}
	inside := d.Stages["inside"]
	inside.Brief = "{{item.missing}}"
	d.Stages["inside"] = inside
	if issues := check(); len(issues) != 1 || !strings.Contains(issues[0], "outside branch element schema") {
		t.Fatal(issues)
	}
	inside.Brief = "{{item.name}}"
	d.Stages["inside"] = inside
	outside := d.Stages["outside"]
	outside.Brief = "{{item.name}}"
	d.Stages["outside"] = outside
	if issues := check(); len(issues) != 1 || !strings.Contains(issues[0], "outside a typed dynamic FanOut") {
		t.Fatal(issues)
	}
}

func TestArtifactBriefReferenceMustBeBoundByStage(t *testing.T) {
	doc := Document{Artifacts: map[string]ArtifactType{"Note": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}}}`)}}, Stages: map[string]Stage{"source": {Primitive: AgentTurn, Output: "Note"}, "reader": {Primitive: AgentTurn, Brief: "{{artifact.Note.text}}"}}}
	check := func() []string {
		var issues []string
		validateBriefTemplates(doc, func(path, message string) { issues = append(issues, path+": "+message) })
		return issues
	}
	if issues := check(); len(issues) != 1 || !strings.Contains(issues[0], "not bound by the stage") {
		t.Fatal(issues)
	}
	reader := doc.Stages["reader"]
	reader.Inputs = []Input{{Name: "note", Type: "Note", Stage: "source", Version: 1}}
	doc.Stages["reader"] = reader
	if issues := check(); len(issues) > 0 {
		t.Fatal(issues)
	}
}

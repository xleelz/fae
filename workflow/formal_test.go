package workflow

import (
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/proof"
	"os"
	"strings"
	"testing"
)

func formalDocument() Document {
	pass := Expr{Op: "eq", Args: []Expr{{Op: "field", Stage: "verify", Path: []string{"passed"}}, {Op: "literal", Value: true}}}
	command := []string{"proof-adapter", "verify", "model.spec"}
	return Document{SchemaVersion: 2, ID: "formal-example", Start: "verify", TargetRequirements: &TargetRequirements{Commands: [][]string{command}}, Limits: Limits{Attempts: 4, TestRuns: 1, ScopeWidens: 1}, Roles: map[string]Role{}, Artifacts: map[string]ArtifactType{"Proof": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["passed","outcome","obligations","verified","failed","unknown","skipped"]}`)}}, Stages: map[string]Stage{"verify": {Primitive: HostCommand, Output: "Proof", Command: command, Weight: 1, TimeoutSeconds: 60, Formal: &proof.Config{Tool: "custom-checker", Parser: "normalized-json", Kind: proof.Deductive, Sources: []string{"model.spec"}}}, "gate": {Primitive: Gate, Name: "formal result accepted", Expr: &pass, Failure: "stop"}, "stop": {Primitive: Escalate, Kind: domain.HumanDecisionRequired, Reason: "formal proof did not pass"}}, Edges: []Edge{{From: "verify", To: "gate"}, {From: "gate", To: "Done"}, {From: "gate", To: "stop"}}}
}
func TestFormalConfigurationAndFailClosedGate(t *testing.T) {
	d := formalDocument()
	if issues := Validate(d); len(issues) > 0 {
		t.Fatal(issues)
	}
	d.Stages["verify"].Formal.Sources = nil
	if issues := Validate(d); len(issues) > 0 {
		t.Fatal("whole-tree source binding rejected", issues)
	}
	if !FormalPositiveGate(d, *d.Stages["gate"].Expr) {
		t.Fatal("genuine passed selector not recognized")
	}
	for _, outcome := range []string{"unknown", "error", "unsupported", "skipped", "disproved"} {
		ev := []Evidence{{ID: "receipt", Stage: "verify", Type: "Proof", Provenance: true, Fields: map[string]any{"outcome": outcome, "passed": false, "obligations": 1, "verified": 0, "failed": 0, "unknown": 1, "skipped": 0}}}
		if Eval(*d.Stages["gate"].Expr, ev).Passed {
			t.Fatal("nonproof passed", outcome)
		}
	}
	if Eval(*d.Stages["gate"].Expr, nil).Passed {
		t.Fatal("missing proof passed")
	}
	ev := []Evidence{{ID: "receipt", Stage: "verify", Type: "Proof", Provenance: true, Fields: map[string]any{"outcome": "proved", "passed": true, "obligations": 2, "verified": 2, "failed": 0, "unknown": 0, "skipped": 0}}}
	if !Eval(*d.Stages["gate"].Expr, ev).Passed {
		t.Fatal("genuine proof rejected")
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ParseWrite(raw)
	if err != nil || decoded.Stages["verify"].Formal == nil {
		t.Fatal(err)
	}
}
func TestFormalRejectsAmbiguityAndExitOnlyGates(t *testing.T) {
	tests := []struct {
		name, contains string
		mutate         func(*Document)
	}{
		{"non-host", "belongs only", func(d *Document) { s := d.Stages["verify"]; s.Primitive = AgentTurn; d.Stages["verify"] = s }},
		{"test-run", "cannot combine", func(d *Document) { s := d.Stages["verify"]; s.TestRun = true; d.Stages["verify"] = s }},
		{"check", "cannot combine", func(d *Document) {
			s := d.Stages["verify"]
			s.Check = &CheckSpec{Format: "lines", CountField: "checks"}
			d.Stages["verify"] = s
		}},
		{"generator", "cannot combine", func(d *Document) {
			s := d.Stages["verify"]
			s.Generate = &GenerateSpec{Writes: []string{"model.spec"}}
			d.Stages["verify"] = s
		}},
		{"escaped-source", "relative paths", func(d *Document) { d.Stages["verify"].Formal.Sources = []string{"../outside.spec"} }},
		{"duplicate-source", "unique literal", func(d *Document) { d.Stages["verify"].Formal.Sources = []string{"model.spec", "model.spec"} }},
		{"unknown-native-tool", "custom formal tool", func(d *Document) { d.Stages["verify"].Formal.Parser = "native" }},
		{"exit-zero", "formal gate must require", func(d *Document) {
			s := d.Stages["gate"]
			s.Expr = &Expr{Op: "eq", Args: []Expr{{Op: "field", Stage: "verify", Path: []string{"exit_code"}}, {Op: "literal", Value: 0}}}
			d.Stages["gate"] = s
		}},
		{"outcome-label", "formal gate must require", func(d *Document) {
			s := d.Stages["gate"]
			s.Expr = &Expr{Op: "eq", Args: []Expr{{Op: "field", Stage: "verify", Path: []string{"outcome"}}, {Op: "literal", Value: "proved"}}}
			d.Stages["gate"] = s
		}},
		{"passing-bypass-arm", "formal gate must require", func(d *Document) {
			s := d.Stages["gate"]
			s.Expr = &Expr{Op: "or", Args: []Expr{*s.Expr, {Op: "literal", Value: true}}}
			d.Stages["gate"] = s
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := formalDocument()
			tt.mutate(&d)
			issues := Validate(d)
			found := false
			for _, issue := range issues {
				found = found || strings.Contains(issue.Message, tt.contains)
			}
			if !found {
				t.Fatal(issues)
			}
		})
	}
}
func TestFormalGateSelectorsAndPassingArms(t *testing.T) {
	field := func(stage, typ string) Expr {
		return Expr{Op: "eq", Args: []Expr{{Op: "field", Stage: stage, Type: typ, Path: []string{"passed"}}, {Op: "literal", Value: true}}}
	}
	a := field("verify", "")
	if !FormalGateAccepts(a, "verify", "") || FormalGateAccepts(a, "other", "") {
		t.Fatal("stage selector conflated")
	}
	byType := field("", "Proof")
	if !FormalGateAccepts(byType, "", "Proof") || FormalGateAccepts(byType, "verify", "") {
		t.Fatal("type selector conflated")
	}
	every := Expr{Op: "or", Args: []Expr{a, {Op: "and", Args: []Expr{a, {Op: "literal", Value: true}}}}}
	if !FormalGateAccepts(every, "verify", "") {
		t.Fatal("valid passing arms rejected")
	}
	bypass := Expr{Op: "or", Args: []Expr{a, {Op: "literal", Value: true}}}
	if FormalGateAccepts(bypass, "verify", "") {
		t.Fatal("bypass admitted")
	}
	d := formalDocument()
	s := d.Stages["gate"]
	s.Expr = &byType
	d.Stages["gate"] = s
	s = d.Stages["stop"]
	s.Output = "Proof"
	d.Stages["stop"] = s
	issues := Validate(d)
	found := false
	for _, issue := range issues {
		found = found || strings.Contains(issue.Message, "non-formal producer")
	}
	if !found {
		t.Fatal(issues)
	}
}

func TestFormalExampleWorkflowIsValid(t *testing.T) {
	raw, err := os.ReadFile("../examples/formal-workflow.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ParseWrite(raw); err != nil {
		t.Fatal(err)
	}
}

func TestFormalProofDoesNotReplaceProtectedTestingFloor(t *testing.T) {
	d := protected()
	s := d.Stages["test"]
	s.TestRun = false
	s.Command = []string{"proof-adapter", "model.spec"}
	s.Formal = &proof.Config{Tool: "custom-checker", Parser: "normalized-json", Kind: proof.Deductive}
	d.Stages["test"] = s
	gate := d.Stages["gate"]
	gate.Expr = &Expr{Op: "eq", Args: []Expr{{Op: "field", Stage: "test", Path: []string{"passed"}}, {Op: "literal", Value: true}}}
	d.Stages["gate"] = gate
	found := false
	for _, issue := range Validate(d) {
		found = found || strings.Contains(issue.Message, "passing test gate")
	}
	if !found {
		t.Fatal("formal result replaced mandatory executed testing floor", Validate(d))
	}
}

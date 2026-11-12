package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCommandAdapterValidationRetainsEvidenceContracts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  Stage
		reject bool
	}{
		{name: "implicit-builtin", stage: Stage{Primitive: HostCommand}},
		{name: "explicit-builtin", stage: Stage{Primitive: HostCommand, CommandAdapter: "builtin"}},
		{name: "configured-check", stage: Stage{Primitive: HostCommand, CommandAdapter: "configured", Command: []string{"new-tool", "--check"}, Check: &CheckSpec{Format: "json", CountField: "checked"}}},
		{name: "configured-generator", stage: Stage{Primitive: HostCommand, CommandAdapter: "configured", Generate: &GenerateSpec{Writes: []string{"generated/**"}}}},
		{name: "unknown-adapter", stage: Stage{Primitive: HostCommand, CommandAdapter: "future-magic"}, reject: true},
		{name: "agent-adapter", stage: Stage{Primitive: AgentTurn, CommandAdapter: "configured"}, reject: true},
		{name: "formal-builtin", stage: func() Stage { s := formalDocument().Stages["verify"]; s.CommandAdapter = "builtin"; return s }(), reject: true},
		{name: "test-without-shape", stage: Stage{Primitive: HostCommand, CommandAdapter: "configured", TestRun: true, Command: []string{"new-tool", "test"}}, reject: true},
		{name: "fake-node-shape", stage: Stage{Primitive: HostCommand, CommandAdapter: "configured", TestRun: true, TestShape: "node-test", Command: []string{"new-tool", "test"}}, reject: true},
		{name: "existing-runner", stage: Stage{Primitive: HostCommand, CommandAdapter: "configured", TestRun: true, TestShape: "runner", Command: []string{"new-test-runner", "test"}}},
		{name: "existing-script", stage: Stage{Primitive: HostCommand, CommandAdapter: "configured", TestRun: true, TestShape: "script", Command: []string{"./scripts/test-one"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := []string{}
			validateCommandAdapters(Document{Stages: map[string]Stage{"run": tc.stage}}, func(path, reason string) { problems = append(problems, path+": "+reason) })
			if (len(problems) > 0) != tc.reject {
				t.Fatal(problems)
			}
		})
	}
}
func TestConfiguredAdapterSurvivesWriteAndIsNotFormalProof(t *testing.T) {
	d := formalDocument()
	stage := d.Stages["verify"]
	stage.Formal = nil
	stage.CommandAdapter = "configured"
	stage.Output = "ExecutionReceipt"
	d.Stages["verify"] = stage
	d.Artifacts = map[string]ArtifactType{"ExecutionReceipt": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["executed","exit_code"]}`)}}
	gate := d.Stages["gate"]
	gate.Expr = &Expr{Op: "and", Args: []Expr{{Op: "eq", Args: []Expr{{Op: "field", Stage: "verify", Path: []string{"executed"}}, {Op: "literal", Value: true}}}, {Op: "eq", Args: []Expr{{Op: "field", Stage: "verify", Path: []string{"exit_code"}}, {Op: "literal", Value: 0}}}}}
	d.Stages["gate"] = gate
	if issues := Validate(d); len(issues) > 0 {
		t.Fatal(issues)
	}
	if FormalPositiveGate(d, *gate.Expr) {
		t.Fatal("plain execution receipt promoted to proof")
	}
	raw, _ := json.Marshal(d)
	decoded, err := ParseWrite(raw)
	if err != nil || decoded.Stages["verify"].CommandAdapter != "configured" {
		t.Fatal("configured adapter not persisted", err)
	}
	stage = d.Stages["verify"]
	stage.CommandAdapter = "mystery"
	d.Stages["verify"] = stage
	issues := Validate(d)
	found := false
	for _, issue := range issues {
		if strings.Contains(issue.Path, "command_adapter") {
			found = true
		}
	}
	if !found {
		t.Fatal("public validation skipped adapter hook", issues)
	}
}

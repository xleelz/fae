package workflow_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
)

func hasIssueAt(issues []workflow.Issue, path, message string) bool {
	for _, i := range issues {
		if i.Path == path && strings.Contains(i.Message, message) {
			return true
		}
	}
	return false
}

// FAE-WF-030 with FAE-WF-093: schema 2 requires a Gate's name and an
// Escalate stage's kind and reason; a stored schema-1 revision without them
// still validates (loads), but a new revision must be written in schema 2.
func TestSchemaTwoRequiresPrimitiveConfiguration(t *testing.T) {
	shipped := defaults.Shipped()
	if shipped.SchemaVersion != workflow.CurrentSchema || len(workflow.ValidateWrite(shipped)) != 0 {
		t.Fatal("shipped default is not current-schema content", workflow.ValidateWrite(shipped))
	}
	d := defaults.Workflow()
	stage(&d, "spec-gate", func(s *workflow.Stage) { s.Name = "" })
	stage(&d, "stop", func(s *workflow.Stage) { s.Kind, s.Reason = "", "" })
	issues := workflow.Validate(d)
	for path, message := range map[string]string{"stages.spec-gate.name": "gate name", "stages.stop.kind": "escalation kind", "stages.stop.reason": "reason template"} {
		if !hasIssueAt(issues, path, message) {
			t.Errorf("schema 2 accepted a missing %s: %v", path, issues)
		}
	}
	d.SchemaVersion = 1
	if issues = workflow.Validate(d); len(issues) != 0 {
		t.Fatal("stored schema-1 content no longer loads", issues)
	}
	if !hasIssueAt(workflow.ValidateWrite(d), "schema_version", "must use schema 2") {
		t.Fatal("a schema-1 revision was writable")
	}
}

// Authoring must enforce current required fields without making an old pinned
// revision unreadable or unrunnable.
func TestParseWriteRejectsLegacyConfigurationButPreservesStoredReads(t *testing.T) {
	d := defaults.Workflow()
	d.SchemaVersion = 1
	stage(&d, "spec-gate", func(s *workflow.Stage) { s.Name = "" })
	stage(&d, "stop", func(s *workflow.Stage) { s.Kind = ""; s.Reason = "" })
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = workflow.Parse(raw); err != nil {
		t.Fatal("legacy pinned revision no longer runs", err)
	}
	if _, err = workflow.Load(raw); err != nil {
		t.Fatal("legacy pinned revision no longer loads", err)
	}
	if _, err = workflow.ParseWrite(raw); err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatal("new legacy content accepted", err)
	}
	d = defaults.Workflow()
	stage(&d, "spec-gate", func(s *workflow.Stage) { s.Name = "" })
	stage(&d, "stop", func(s *workflow.Stage) { s.Kind = ""; s.Reason = "" })
	raw, _ = json.Marshal(d)
	_, err = workflow.ParseWrite(raw)
	for _, path := range []string{"stages.spec-gate.name", "stages.stop.kind", "stages.stop.reason"} {
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("missing issue %s: %v", path, err)
		}
	}
}

func TestNewRevisionPinsSchemaRetryDefaultWithoutChangingStoredReads(t *testing.T) {
	raw, _ := json.Marshal(defaults.Workflow())
	stored, err := workflow.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	authored, err := workflow.ParseWrite(raw)
	if err != nil {
		t.Fatal(err)
	}
	for id, s := range stored.Stages {
		if s.Primitive == workflow.AgentTurn && s.SchemaRetries == nil {
			if n := authored.Stages[id].SchemaRetries; n == nil || *n != 1 {
				t.Fatal("unbound retry default", id, n)
			}
		}
	}
}

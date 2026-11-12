package daemon

import (
	"encoding/json"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
)

// FAE-WF-093 with FAE-WF-041: the stored-revision check accepts a revision
// with a Claude minimal route, which a revision written before the rule may
// hold, while the install check and the validate verb refuse it.
func TestWorkflowWriteRefusesClaudeMinimalThatAStoredRevisionPasses(t *testing.T) {
	_, raw := claudeMinimalWorkflow("minimal")
	if err := validWorkflow(raw); err != nil {
		t.Fatal("a stored revision with a Claude minimal route is refused:", err)
	}
	if err := validWorkflowWrite(raw); err == nil || !strings.Contains(err.Error(), "roles.qa.routes[0]: "+claudeMinimalIssue) {
		t.Fatal("install accepts a Claude minimal route:", err)
	}
	v := ValidateWorkflowContent(raw)
	if v.Valid || v.Workflow != "minimal" || len(v.Issues) != 1 || v.Issues[0].Path != "roles.qa.routes[0]" || v.ContentHash != "" {
		t.Fatalf("validate %+v", v)
	}
	// Every issue is listed in one refusal, the write rule among them.
	broken, _ := claudeMinimalWorkflow("minimal")
	broken.Limits.Attempts = 0
	raw, _ = json.Marshal(broken)
	err := validWorkflowWrite(raw)
	if err == nil || !strings.Contains(err.Error(), claudeMinimalIssue) || !strings.Contains(err.Error(), "limits.attempts") {
		t.Fatal("one refusal does not list every issue:", err)
	}
	if v = ValidateWorkflowContent(raw); v.Valid || len(v.Issues) != 2 {
		t.Fatalf("validate %+v", v)
	}
}

// New authoring paths enforce current required configuration while stored
// revisions retain their original loading/running contract.
func TestWorkflowWriteRequiresCurrentSchemaWithoutBreakingPinnedReads(t *testing.T) {
	doc := defaults.Workflow()
	doc.SchemaVersion = 1
	gate := doc.Stages["spec-gate"]
	gate.Name = ""
	doc.Stages["spec-gate"] = gate
	stop := doc.Stages["stop"]
	stop.Kind = ""
	stop.Reason = ""
	doc.Stages["stop"] = stop
	raw, _ := json.Marshal(doc)
	if err := validWorkflow(raw); err != nil {
		t.Fatal("stored legacy revision refused", err)
	}
	if err := validWorkflowWrite(raw); err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatal("new legacy revision accepted", err)
	}
	if v := ValidateWorkflowContent(raw); v.Valid || len(v.Issues) == 0 || v.Issues[0].Path != "schema_version" {
		t.Fatalf("offline validation %+v", v)
	}
	doc.SchemaVersion = workflow.CurrentSchema
	raw, _ = json.Marshal(doc)
	for _, validate := range []func([]byte) error{validWorkflowWrite} {
		err := validate(raw)
		for _, path := range []string{"stages.spec-gate.name", "stages.stop.kind", "stages.stop.reason"} {
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Errorf("missing issue %s: %v", path, err)
			}
		}
	}
}

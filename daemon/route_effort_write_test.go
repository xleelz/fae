package daemon

import (
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
)

// claudeMinimalWorkflow is the default workflow with its qa role's first
// route bound to a Claude minimal effort, which Claude Code doesn't have.
func claudeMinimalWorkflow(id string) (workflow.Document, []byte) {
	doc := defaults.Workflow()
	doc.ID = id
	qa := doc.Roles["qa"]
	qa.Routes = append([]workflow.Binding{{Provider: "claude", Model: "claude-opus-5-5", Effort: "minimal"}}, qa.Routes[1:]...)
	doc.Roles["qa"] = qa
	raw, _ := json.Marshal(doc)
	return doc, raw
}

const claudeMinimalIssue = "effort minimal is not supported by provider claude"

// FAE-WF-093: a stored revision with a Claude minimal route still loads, so a
// task can be admitted on it and it can be read and simulated; installing one
// is refused. At run time the bridge refuses that binding before Claude runs
// (TestClaudePreflightRefusesBeforeLaunch), and the turn moves to the next
// route (TestDaemonBindingRefusalFailsOverToTheNextRoute).
func TestStoredClaudeMinimalRevisionAdmitsAndInstallRefusesIt(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	doc, raw := claudeMinimalWorkflow("stored-minimal")
	refused(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, "", claudeMinimalIssue)
	// A revision written before the rule existed.
	audit := controlplane.Audit{Actor: "test", Action: "store a pre-wave-9 revision", Outcome: "ok"}
	if _, err = d.store.WriteWorkflow(ctx, audit, doc.ID, doc.ID, raw, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got := governanceCall[WorkflowRead](t, d, "Workflows", WorkflowParams{ID: doc.ID})
	if _, err = workflow.Parse(got.Document); err != nil {
		t.Fatal("stored revision does not parse:", err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "on-minimal", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "on-minimal"); err != nil {
		t.Fatal("admission on a stored Claude minimal revision refused:", err)
	}
	// The stored revision loads for simulate: an empty scenario runs until it
	// has no result for the first stage.
	refused(t, d, "WorkflowSimulate", WorkflowParams{ID: doc.ID, Scenario: json.RawMessage(`{}`), Task: "sim"}, "", "missing script result for intake")
}

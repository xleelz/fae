package daemon

import (
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// unrunnableLegacyDocument is a stored revision this binary reads but cannot
// run: the old default with `main` brief keys (a rule added after it was
// written), under the workflow id given.
func unrunnableLegacyDocument(id string) workflow.Document {
	old := defaults.Workflow()
	old.ID = id
	for name, role := range old.Roles {
		briefs := map[string]string{}
		for _, text := range role.Briefs {
			briefs["main"] = text
		}
		role.Briefs = briefs
		old.Roles[name] = role
	}
	return old
}

// plantPinnedTask stores, as an older binary would have admitted it, a
// Pending scripted task pinned to a stored revision whose document it holds.
func plantPinnedTask(t *testing.T, d *Daemon, cfg Config, id string, workflowID string, revision int64, doc workflow.Document) {
	t.Helper()
	ctx := t.Context()
	pinned, err := engine.New(id, int(revision), defaults.Shipped(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	pinned.Document = doc
	if pinned.WorkflowHash, err = workflow.Hash(doc); err != nil || engine.Validate(pinned) != nil {
		t.Fatal(err)
	}
	pinned.Disposition = domain.Disposition{Mode: domain.Local}
	payload, _ := json.Marshal(envelope{Job: pinned, Admission: AdmitParams{ID: id, RunID: cfg.RunID, Mode: "Local", Priority: 10, WorkflowID: workflowID, Revision: revision}})
	if _, err = d.store.AdmitJob(ctx, controlplane.Audit{Actor: "test", Action: "older binary admission", Outcome: "ok"}, id, controlplane.JobSnapshot{ID: id, WorkflowID: workflowID, WorkflowRevision: revision, TargetID: cfg.Workspace, TargetRevision: 1, State: string(pinned.State), Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

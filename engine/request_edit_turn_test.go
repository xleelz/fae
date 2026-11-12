package engine_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// FAE-WF-078 with FAE-VCS-003: a live change turn whose request is malformed
// (here a widen into a nested .git) is refused as MalformedOutput, but its
// edits, which passed the change checks, are recorded as the task's tree. The
// retry dispatches on that tree, so it does not escalate StaleBranchResidue,
// and the checkpoint matches the workspace, so restart and Resume are not
// refused. Repeated, the refusal still ends SameGate.
func TestMalformedRequestInAnEditTurnRecordsItsEdits(t *testing.T) {
	doc := requestWorkflow()
	work := doc.Stages["work"]
	work.ChangeProducer = true
	doc.Stages["work"] = work
	j, err := engine.NewLive("task", 1, doc, "base-tree")
	if err != nil {
		t.Fatal(err)
	}
	dispatch := func(j engine.Job) (engine.Job, *engine.Dispatch) {
		t.Helper()
		for range 3 {
			next, d, _, err := engine.Prepare(j)
			if err != nil {
				t.Fatal(err)
			}
			j = next
			if d != nil {
				return j, d
			}
		}
		t.Fatal("no dispatch", j.State, j.Reason)
		return j, nil
	}
	bad := json.RawMessage(`{"request":{"kind":"scope-widen","files":["pkg/extra.go","sub/.git/config"],"reason":"x"}}`)
	for i, tree := range []string{"edit-1", "edit-2", "edit-3", "edit-4"} {
		var d *engine.Dispatch
		j, d = dispatch(j)
		if d.Stage != "work" || (i > 0 && d.Tree != []string{"edit-1", "edit-2", "edit-3"}[i-1]) {
			t.Fatalf("turn %d dispatched at %s on tree %s, not on the previous turn's edits", i, d.Stage, d.Tree)
		}
		r := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now(), Output: bad, NewTree: tree}
		if j, _, err = engine.Complete(j, d.ID, r); err != nil {
			t.Fatal(err)
		}
		j = roundTrip(t, j)
		if err = engine.Validate(j); err != nil {
			t.Fatal(err)
		}
		if j.Tree != tree || !strings.Contains(j.Reason, "MalformedOutput") || !strings.Contains(j.Reason, "inside .git") || len(j.ApprovedScope) != 0 || j.Counts["judge"] != 0 {
			t.Fatalf("turn %d: tree %s reason %q scope %v: the malformed request's edits were not recorded", i, j.Tree, j.Reason, j.ApprovedScope)
		}
	}
	if j.State != domain.Escalated || j.EscalationKind != domain.SameGate {
		t.Fatal("a repeated malformed request did not end SameGate", j.State, j.EscalationKind, j.Reason)
	}
}

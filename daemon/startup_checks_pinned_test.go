package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
)

// FAE-WF-093: the start's refusal for a stored revision it cannot load names
// the active tasks pinned to it, and its daemon line names them with the
// recovery; tasks on other revisions are not named.
func TestStartupRevisionCheckNamesPinnedTasks(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	if _, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "plant a revision this binary cannot load", Outcome: "ok"}, "broken", "broken", json.RawMessage(`{"schema_version":3,"id":"broken","start":"nowhere","from_a_later_binary":true}`), func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, err = d.startupRevisionCheck(ctx, []controlplane.JobSnapshot{{ID: "pinned-task", WorkflowID: "broken", WorkflowRevision: 1}, {ID: "other", WorkflowID: "default", WorkflowRevision: 1}}, nil)
	if err == nil || !strings.Contains(err.Error(), "workflow broken revision 1") || !strings.Contains(err.Error(), "active tasks [pinned-task] are pinned to it") || strings.Contains(err.Error(), "other") {
		t.Fatalf("the refusal does not name exactly the pinned task: %v", err)
	}
	log, _ := os.ReadFile(d.layout.Log)
	if !strings.Contains(string(log), "active tasks [pinned-task] are pinned to it; it is the workflow's latest revision, which new admissions bind; to recover: every stored revision must stay loadable") {
		t.Fatal("the daemon line does not name the pinned tasks and the recovery")
	}
}

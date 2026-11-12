package controlplane

import (
	"context"
	"encoding/json"
	"github.com/DATA-DOG/go-sqlmock"
	"testing"
)

func expectTaskEventMetadata(m sqlmock.Sqlmock) {
	m.ExpectQuery("SELECT x.parent_task,x.tree_root,t.run_id,j.workflow_id").WillReturnRows(sqlmock.NewRows([]string{"parent", "root", "run", "workflow"}).AddRow("", "root", "run", "wf"))
}

func TestStoredTaskEventRetainsWriteTimeMetadata(t *testing.T) {
	s, _ := fixture(t, 1)
	raw, _ := json.Marshal(storedTaskEvent{Version: 1, Metadata: eventMetadata{Task: "child", Parent: "old-parent", Root: "old-root", Run: "run", Workflow: "wf"}, Payload: json.RawMessage(`{"value":1}`)})
	events, err := s.restoreEventMetadata(context.Background(), []Event{{JobID: "child", Payload: raw}})
	if err != nil || events[0].ParentTask != "old-parent" || events[0].TreeRoot != "old-root" || string(events[0].Payload) != `{"value":1}` {
		t.Fatal(events, err)
	}
}

func TestLegacyLargeEventReferenceIsNotExpandedForMetadata(t *testing.T) {
	s, _ := fixture(t, 1)
	raw, _ := json.Marshal(map[string]BlobRef{"blob_ref": {Digest: Digest([]byte("legacy")), Size: 2 << 20}})
	events, err := s.restoreEventMetadata(context.Background(), []Event{{JobID: "legacy", Payload: raw}})
	if err != nil || len(events) != 1 || string(events[0].Payload) != string(raw) || events[0].TreeRoot != "" {
		t.Fatal(events, err)
	}
}

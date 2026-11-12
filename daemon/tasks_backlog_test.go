package daemon

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
)

// A backlog admission's keyed upsert (Dolt) replaces the structured fields
// its item names and records a task-edited event naming every changed
// field; an edge edit records a backlog_edge_changed event naming the edge
// on both of its tasks (daemon/TASKS.md: each edit is in the task's trace).
func TestBacklogUpsertAndEdgeEditAreRecordedEdits(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	doc := shortWorkflow()
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	item := func(id, content string, files ...string) BacklogItemParams {
		return BacklogItemParams{AdmitParams: AdmitParams{ID: id, WorkflowID: doc.ID, Tree: "tree", Priority: 10, Content: content, DeclaredFiles: files}}
	}
	admit := func(items ...BacklogItemParams) {
		t.Helper()
		if _, err := call(t, d, "BacklogAdmit", BacklogParams{Items: items, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	admit(item("a", "first", "docs/a.md"), item("b", "other"))
	edited := item("a", "second", "docs/b.md")
	edited.TestFilter = "^check"
	admit(edited)
	var e envelope
	s, err := d.store.GetJob(ctx, "a")
	if err == nil {
		err = decodeEnvelope(s.Payload, &e)
	}
	if err != nil || !reflect.DeepEqual(e.Admission.DeclaredFiles, []string{"docs/b.md"}) || e.Admission.TestFilter != "^check" || e.Admission.Content != "second" {
		t.Fatalf("upsert left the structured fields: %v %q %v", e.Admission.DeclaredFiles, e.Admission.TestFilter, err)
	}
	eventsOf := func(id, kind string) []string {
		t.Helper()
		events, err := d.store.JobEvents(ctx, id, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, ev := range events {
			if ev.Kind == kind {
				out = append(out, string(ev.Payload))
			}
		}
		return out
	}
	edits := eventsOf("a", "task-edited")
	if len(edits) != 1 || !strings.Contains(edits[0], `"fields":["content","declared_files","test_filter"]`) || !strings.Contains(edits[0], `"via":"BacklogAdmit"`) || !strings.Contains(edits[0], `"acceptance_sha256":"`+controlplane.Digest([]byte("second"))+`"`) {
		t.Fatalf("upsert edit events %v", edits)
	}
	edge := BacklogEdgeParams{BacklogEdge: controlplane.BacklogEdge{From: "a", To: "b", Kind: "blocking"}, AuthorizingMessage: "message"}
	if _, err = call(t, d, "BacklogEdge", edge, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		changed := eventsOf(id, "backlog_edge_changed")
		var detail map[string]any
		if len(changed) != 1 || json.Unmarshal([]byte(changed[0]), &detail) != nil || !reflect.DeepEqual(detail, map[string]any{"from": "a", "to": "b", "kind": "blocking", "removed": false}) {
			t.Fatalf("edge edit events of %s: %v", id, changed)
		}
	}
}

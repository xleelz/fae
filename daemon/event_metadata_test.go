package daemon

import (
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"strings"
	"testing"
	"time"
)

func TestEventMetadataNativeRemainsAtWriteAfterTaskTreeChanges(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.InlineLimit = controlplane.MinInlineLimit
	cfg.Executor = engine.NewScripted(defaults.HappyScript())
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "original-root", Tree: "tree", RunID: "original-run", AuthorizingMessage: "message"}, "original-root"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "original-root", "Done")
	if err = d.store.RecordTaskEvent(ctx, controlplane.Audit{Actor: "daemon", Action: "metadata-before", Outcome: "ok"}, "original-root", controlplane.Event{Kind: "metadata-before", Payload: json.RawMessage(`{"value":1}`)}); err != nil {
		t.Fatal(err)
	}
	bigBody, _ := json.Marshal(map[string]string{"value": strings.Repeat("payload-value-", 4096)})
	if err = d.store.RecordTaskEvent(ctx, controlplane.Audit{Actor: "daemon", Action: "metadata-big", Outcome: "ok"}, "original-root", controlplane.Event{Kind: "metadata-big", Payload: bigBody}); err != nil {
		t.Fatal(err)
	}
	row, err := d.store.GetJob(ctx, "original-root")
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err = decodeEnvelope(row.Payload, &e); err != nil {
		t.Fatal(err)
	}
	e.Job.TreeRoot = "changed-root"
	e.Job.TreeDepth = 2
	e.Job.Parent = &engine.ParentRef{Task: "changed-parent", Detached: true}
	e.Admission.RunID = "changed-run"
	row.Payload, _ = json.Marshal(e)
	row.Events = []controlplane.Event{{Kind: "metadata-after", Payload: json.RawMessage(`{}`)}}
	if _, err = d.store.SaveJob(ctx, controlplane.Audit{Actor: "daemon", Action: "metadata-fixture-change", Outcome: "ok"}, row, row.Version); err != nil {
		t.Fatal(err)
	}
	verify := func(rows []controlplane.Event) {
		t.Helper()
		found := false
		for _, event := range rows {
			if event.Kind != "metadata-before" {
				continue
			}
			found = true
			if event.TreeRoot != "original-root" || event.ParentTask != "" || event.RunID != "original-run" || event.WorkflowID != row.WorkflowID || string(event.Payload) != `{"value":1}` {
				t.Fatalf("history metadata followed current task: %+v", event)
			}
		}
		if !found {
			t.Fatal("missing original event")
		}
	}
	bigEvent, err := d.store.LastJobEvent(ctx, "original-root", "metadata-big")
	if err != nil {
		t.Fatal(err)
	}
	if len(bigEvent.Payload) > controlplane.MinInlineLimit || !strings.Contains(string(bigEvent.Payload), `"blob_ref"`) {
		t.Fatal("metadata restore expanded spilled event", len(bigEvent.Payload))
	}
	resolved, err := d.store.ResolveText(ctx, string(bigEvent.Payload))
	if err != nil || string(resolved) != string(bigBody) || bigEvent.TreeRoot != "original-root" || bigEvent.RunID != "original-run" {
		t.Fatal("spilled body or write metadata changed", bigEvent, err)
	}
	events, err := d.store.JobEvents(ctx, "original-root", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	verify(events)
	last, err := d.store.LastJobEvent(ctx, "original-root", "metadata-before")
	if err != nil {
		t.Fatal(err)
	}
	verify([]controlplane.Event{last})
	events, err = d.store.TaskEventsOfKinds(ctx, "original-root", []string{"metadata-before"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	verify(events)
	events, _, err = d.store.EventsOfKindSince(ctx, "metadata-before", time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	verify(events)
	events, _, err = d.store.EventsSince(ctx, time.Time{}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	verify(events)
	response, err := call(t, d, "Events", map[string]any{"tree_root": "original-root", "kind": "metadata-before", "limit": 10}, "")
	if err != nil {
		t.Fatal(err)
	}
	var page []controlplane.Event
	if err = json.Unmarshal(response.Result, &page); err != nil {
		t.Fatal(err)
	}
	verify(page)
}

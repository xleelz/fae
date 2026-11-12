package daemon

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/symphony"
)

func TestLinearBridgeIdentityAndStatus(t *testing.T) {
	id, e := canonicalLinearID("BE9B7FD9-7AC3-449A-B3E8-42C54A8AEB7A")
	if e != nil || id != "be9b7fd9-7ac3-449a-b3e8-42c54a8aeb7a" {
		t.Fatalf("%q %v", id, e)
	}
	if linearStableTaskID(id) == linearStableTaskID("another-uuid") {
		t.Fatal("colliding issue task identities")
	}
	for _, bad := range []string{"", "../issue", "issue\nother"} {
		if _, e = canonicalLinearID(bad); e == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	for _, state := range []string{"Done", "Cancelled", "Retired", "Escalated", "Blocked", "spec"} {
		s := linearStatus(controlplane.JobSnapshot{ID: "task", State: state, Version: 7}, "ENG-1")
		if s.Completed != (state == "Done") || s.Terminal != backlogTerminal(state) || s.NeedsOperator != (state == "Escalated" || state == "Blocked") || s.Version != 7 || s.Identifier != "ENG-1" {
			t.Fatalf("%+v", s)
		}
	}
	raw, _ := json.Marshal(envelope{Job: engine.Job{State: domain.State("decision"), Parked: true, Reason: engine.ParkHumanDecision}})
	if !linearStatus(controlplane.JobSnapshot{ID: "task", State: "decision", Payload: raw}, "").NeedsOperator {
		t.Fatal("human decision not reported")
	}
}
func TestLinearBridgeDurableBinding(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	if _, e = call(t, d, "TargetConfigPut", TargetPutParams{ID: "linear-target", Document: targetFixture(cfg.Workspace), AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	bridge := d.LinearTaskBridge()
	bridge.config = symphony.FaeConfig{WorkflowID: defaults.Workflow().ID, TargetConfig: "linear-target", AuthorizingMessage: "message", Mode: "Local"}
	issue := symphony.Issue{ID: "11111111-1111-4111-8111-111111111111", Identifier: "ENG-1", Title: "durable issue"}
	bridge.config.AuthorizingMessage = "missing-citation"
	if _, err := bridge.admitLinearIssueDurable(ctx, issue, "/unused", "title", "acceptance", 10); err == nil {
		t.Fatal("missing persistent operator authority accepted")
	}
	bridge.config.AuthorizingMessage = "message"
	results := make(chan string, 8)
	errs := make(chan error, 8)
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			id, e := bridge.admitLinearIssueDurable(ctx, issue, "/unused-workspace", "title", "Implement the scoped fixture acceptance", 10)
			if e != nil {
				errs <- e
			} else {
				results <- id
			}
		}()
	}
	group.Wait()
	close(errs)
	close(results)
	for e := range errs {
		t.Fatal(e)
	}
	taskID := ""
	for id := range results {
		if taskID == "" {
			taskID = id
		}
		if id != taskID {
			t.Fatalf("duplicate task identities %s %s", taskID, id)
		}
	}
	if taskID != linearStableTaskID(issue.ID) {
		t.Fatal(taskID)
	}
	d.mu.Lock()
	graph, e := d.store.Backlog(ctx)
	d.mu.Unlock()
	if e != nil || len(graph.Items) != 1 || graph.Items[0].Source != linearSource(issue.ID) {
		t.Fatalf("%+v %v", graph, e)
	}
	// The UUID binding survives identifier renames without mutating acceptance.
	issue.Identifier = "NEW-9"
	again, e := bridge.admitLinearIssueDurable(ctx, issue, "/unused", "renamed title", "new text must not silently replace accepted work", 30)
	if e != nil || again != taskID {
		t.Fatalf("%q %v", again, e)
	}
	status, found, e := bridge.LookupLinearTask(ctx, issue.ID)
	if e != nil || !found || status.Identifier != "NEW-9" {
		t.Fatalf("%+v %t %v", status, found, e)
	}
	// Simulate the only non-atomic gap: task+backlog source committed but marker
	// absent. A fresh bridge recovers the index instead of admitting another task.
	audit := controlplane.Audit{Actor: "test", Action: "LinearBindingCrashGap", Outcome: "ok"}
	if e = d.store.DeleteMarker(ctx, audit, linearBindingMarker(issue.ID)); e != nil {
		t.Fatal(e)
	}
	bridge = d.LinearTaskBridge()
	bridge.config = symphony.FaeConfig{WorkflowID: defaults.Workflow().ID, TargetConfig: "linear-target", AuthorizingMessage: "message", Mode: "Local"}
	if status, found, e = bridge.LookupLinearTask(ctx, issue.ID); e != nil || !found || status.TaskID != taskID {
		t.Fatalf("gap lookup %+v %t %v", status, found, e)
	}
	again, e = bridge.admitLinearIssueDurable(ctx, issue, "/unused", "title", "acceptance", 10)
	if e != nil || again != taskID {
		t.Fatalf("gap recovery %q %v", again, e)
	}
	if _, e = d.store.GetMarker(ctx, linearBindingMarker(issue.ID)); e != nil {
		t.Fatal(e)
	}
	other := issue
	other.ID = "22222222-2222-4222-8222-222222222222"
	otherID, e := bridge.admitLinearIssueDurable(ctx, other, "/unused", "title", "Different UUID even with same identifier", 10)
	if e != nil || otherID == taskID {
		t.Fatalf("distinct UUID %q %v", otherID, e)
	}
	// Identifier-only old bindings cannot be guessed to belong to a new UUID.
	legacy := BacklogItemParams{AdmitParams: AdmitParams{ID: "legacy-linear", Tree: "tree", WorkflowID: defaults.Workflow().ID, TargetConfig: "linear-target", Title: "legacy", Content: "legacy acceptance", Priority: 10, AuthorizingMessage: "message"}, Source: "linear/ENG-999"}
	if _, e = call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{legacy}, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	collision := issue
	collision.ID = "33333333-3333-4333-8333-333333333333"
	collision.Identifier = "ENG-999"
	if _, e = bridge.admitLinearIssueDurable(ctx, collision, "/unused", "title", "acceptance", 10); e == nil || !strings.Contains(e.Error(), "explicit UUID migration") {
		t.Fatalf("legacy collision: %v", e)
	}
	if _, err := controllerCall(t, d, controller.Params{Action: "rearm", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, taskID, "Done")
	before, found, e := bridge.LookupLinearTask(ctx, issue.ID)
	if e != nil || !found || !before.Completed {
		t.Fatalf("%+v %t %v", before, found, e)
	}
	again, e = bridge.admitLinearIssueDurable(ctx, issue, "/unused", "title", "acceptance", 10)
	if e != nil || again != taskID {
		t.Fatalf("terminal task readmitted %q %v", again, e)
	}
	after, _, e := bridge.LookupLinearTask(ctx, issue.ID)
	if e != nil || after.Version != before.Version {
		t.Fatalf("terminal poll mutated task %+v %+v %v", before, after, e)
	}
	stop(t, d)
	d = nil
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	bridge = d.LinearTaskBridge()
	bridge.config = symphony.FaeConfig{WorkflowID: defaults.Workflow().ID, TargetConfig: "linear-target", AuthorizingMessage: "message", Mode: "Local"}
	all, e := bridge.ListLinearTasks(ctx)
	if e != nil || len(all) != 2 || !all[issue.ID].Completed || all[issue.ID].TaskID != taskID {
		t.Fatalf("restored %+v %v", all, e)
	}
	// An index that points at another task cannot override the atomic source.
	corrupted := linearTaskBinding{Version: 1, IssueID: issue.ID, TaskID: otherID, Source: linearSource(issue.ID)}
	d.mu.Lock()
	e = bridge.writeLinearBindingLocked(ctx, corrupted, audit)
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = bridge.LookupLinearTask(ctx, issue.ID); e == nil {
		t.Fatal("accepted mismatched binding index")
	}
}

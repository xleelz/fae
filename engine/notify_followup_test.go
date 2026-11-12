package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

func TestEngineNotifyNextTurnAndInFlightAndHistory(t *testing.T) {
	now := time.Date(2026, 11, 5, 1, 10, 0, 0, time.UTC)
	target, err := engine.New("task-target", 1, defaults.Workflow(), "tree-1")
	if err != nil {
		t.Fatal(err)
	}
	source, err := engine.New("task-source", 1, defaults.Workflow(), "tree-1")
	if err != nil {
		t.Fatal(err)
	}

	// (a) Notify across all 3 kinds ("breaking-change", "cross-area-need", "note") and delivery modes ("next-turn", "in-flight").
	var n1, n2, n3 engine.TaskNotification
	target, n1, _, err = engine.ApplyNotify(target, engine.TaskNotification{
		ID:         "n-break",
		FromTaskID: "task-source",
		Kind:       "breaking-change",
		Delivery:   "next-turn",
		Body:       "API changed: Foo(ctx) -> Foo(ctx, opts)",
	}, now)
	if err != nil {
		t.Fatalf("ApplyNotify breaking-change: %v", err)
	}
	source, _, err = engine.RecordOutboundNotification(source, n1)
	if err != nil {
		t.Fatalf("RecordOutboundNotification 1: %v", err)
	}

	target, n2, _, err = engine.ApplyNotify(target, engine.TaskNotification{
		ID:         "n-need",
		FromTaskID: "task-source",
		Kind:       "cross-area-need",
		Delivery:   "next-turn",
		Body:       "Need exported Bar() helper in pkg/bar",
	}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("ApplyNotify cross-area-need: %v", err)
	}
	source, _, err = engine.RecordOutboundNotification(source, n2)
	if err != nil {
		t.Fatalf("RecordOutboundNotification 2: %v", err)
	}

	// (d) Controller-relayed "note" across peer tasks without an operator message.
	target, n3, _, err = engine.ApplyNotify(target, engine.TaskNotification{
		ID:         "n-note",
		FromTaskID: "task-source",
		Kind:       "note",
		Delivery:   "next-turn",
		Body:       "Peer note: <script>alert(1)</script> found in legacy parser",
		RelayedBy:  "controller",
	}, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("ApplyNotify controller note: %v", err)
	}
	source, _, err = engine.RecordOutboundNotification(source, n3)
	if err != nil {
		t.Fatalf("RecordOutboundNotification 3: %v", err)
	}

	// (c) Verify both source and target task histories record the notifications.
	if len(source.History) != 3 {
		t.Fatalf("expected 3 outbound history entries on source task, got %d", len(source.History))
	}
	for _, h := range source.History {
		if h.Kind != "task-notification" || !strings.Contains(h.Cause, "to task-target") {
			t.Fatalf("unexpected source history entry: %+v", h)
		}
	}
	if len(target.History) != 3 || len(target.Notifications) != 3 {
		t.Fatalf("expected 3 inbound notifications on target task, got history=%d notifs=%d", len(target.History), len(target.Notifications))
	}

	// (b) Prepare binds undelivered notifications as quoted data into Dispatch.Notifications and Dispatch.OperatorContext.
	target, disp, _, err := engine.Prepare(target)
	if err != nil {
		t.Fatalf("Prepare 1: %v", err)
	}
	if disp == nil {
		target, disp, _, err = engine.Prepare(target)
		if err != nil {
			t.Fatalf("Prepare 2: %v", err)
		}
	}
	if disp == nil {
		t.Fatalf("expected non-nil dispatch after Prepare")
	}
	if len(disp.Notifications) != 3 {
		t.Fatalf("expected 3 notifications bound on dispatch, got %d", len(disp.Notifications))
	}
	if !strings.Contains(disp.OperatorContext, `<notifications data_only="true">`) ||
		!strings.Contains(disp.OperatorContext, `from_task="task-source"`) ||
		!strings.Contains(disp.OperatorContext, `&lt;script&gt;alert(1)&lt;/script&gt;`) {
		t.Fatalf("expected quoted notification block in OperatorContext, got: %s", disp.OperatorContext)
	}

	// Verify in-flight preemption & immediate resume with a newly arrived in-flight notification.
	target, nInFlight, _, err := engine.ApplyNotify(target, engine.TaskNotification{
		ID:         "n-inflight",
		FromTaskID: "task-source",
		Kind:       "breaking-change",
		Delivery:   "in-flight",
		Body:       "Urgent in-flight update",
	}, now.Add(4*time.Second))
	if err != nil {
		t.Fatalf("ApplyNotify in-flight: %v", err)
	}
	checkpointedJob, _, err := engine.ResumeInFlightWithNotification(target, disp.ID)
	if err != nil {
		t.Fatalf("ResumeInFlightWithNotification: %v", err)
	}
	_, resumedDisp, _, err := engine.Prepare(checkpointedJob)
	if err != nil || resumedDisp == nil {
		t.Fatalf("Prepare after in-flight resume: disp=%v err=%v", resumedDisp, err)
	}
	if len(resumedDisp.Notifications) != 4 {
		t.Fatalf("expected resumed dispatch to carry all 3 notifications from preempted turn plus 1 in-flight notification (4 total), got %d: %+v", len(resumedDisp.Notifications), resumedDisp.Notifications)
	}
	for _, wantID := range []string{n1.ID, n2.ID, n3.ID, nInFlight.ID} {
		found := false
		for _, n := range resumedDisp.Notifications {
			if n.ID == wantID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected resumed dispatch to carry notification %s, got %+v", wantID, resumedDisp.Notifications)
		}
	}
	if !strings.Contains(resumedDisp.OperatorContext, "Urgent in-flight update") ||
		!strings.Contains(resumedDisp.OperatorContext, "API changed: Foo(ctx) -&gt; Foo(ctx, opts)") {
		t.Fatalf("expected resumed OperatorContext to contain both preempted and in-flight notifications, got: %s", resumedDisp.OperatorContext)
	}
}

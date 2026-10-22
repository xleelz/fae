package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"strings"
	"testing"
	"time"
)

func awaitDrive(t *testing.T, d *Daemon, alive bool, generation uint64) controller.Health {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h, e := controllerCall(t, d, controller.Params{Action: "health", AuthorizingMessage: "message"})
		if e != nil {
			t.Fatal(e)
		}
		if h.DriveAlive == alive && h.DriveGeneration >= generation {
			return h
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("drive state timeout")
	return controller.Health{}
}

func TestIndependentDriveRetainsInFlightAndDurableBudget(t *testing.T) {
	cfg, ctx := fixture(t)
	gated := &drainExecutor{started: make(chan string, 100), release: make(chan struct{}), delegate: engine.NewScripted(defaults.HappyScript())}
	cfg.Executor = gated
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { stop(t, d) })
	authorize(t, d)
	h := awaitDrive(t, d, true, 1)
	if _, e = controllerCall(t, d, controller.Params{Action: "rearm", AuthorizingMessage: "message"}); e != nil {
		t.Fatal(e)
	}
	if d.driveGeneration.Load() != h.DriveGeneration {
		t.Fatal("alive rearm duplicated generation")
	}
	if _, e = call(t, d, "Admit", AdmitParams{ID: "work", Tree: "tree", AuthorizingMessage: "message"}, "work"); e != nil {
		t.Fatal(e)
	}
	select {
	case <-gated.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); e != nil {
		t.Fatal(e)
	}
	stopped := awaitDrive(t, d, false, 1)
	if stopped.PID != h.PID || stopped.DoltPID != h.DoltPID {
		t.Fatal("drive stop replaced process")
	}
	d.mu.Lock()
	before := d.jobs["work"]
	active := d.active["work"]
	d.mu.Unlock()
	if !active {
		t.Fatal("active dispatch forgotten")
	}
	close(gated.release)
	time.Sleep(100 * time.Millisecond)
	d.mu.Lock()
	unchanged := d.jobs["work"].Version == before.Version
	d.mu.Unlock()
	if !unchanged {
		t.Fatal("stopped generation collected work")
	}
	for i := 0; i < 3; i++ {
		if _, e = controllerCall(t, d, controller.Params{Action: "rearm", Cause: "forged-" + string(rune('a'+i)), AuthorizingMessage: "message"}); e != nil {
			t.Fatal(e)
		}
		awaitDrive(t, d, true, uint64(i+2))
		if i == 0 {
			awaitState(t, d, "work", "Done")
		}
		if _, e = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); e != nil {
			t.Fatal(e)
		}
		awaitDrive(t, d, false, uint64(i+2))
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "rearm", Cause: "new-cause", AuthorizingMessage: "message"}); e == nil || !strings.Contains(e.Error(), "budget") {
		t.Fatal("budget bypass", e)
	}
	raw, e := d.store.GetMarker(ctx, "drive-rearm/operator-stop")
	if e != nil {
		t.Fatal(e)
	}
	var b driveBudget
	if e = json.Unmarshal(raw, &b); e != nil || b.Count != 3 {
		t.Fatal(b, e)
	}
	stop(t, d)
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	awaitDrive(t, d, true, 1)
	if _, e = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); e != nil {
		t.Fatal(e)
	}
	awaitDrive(t, d, false, 1)
	if _, e = controllerCall(t, d, controller.Params{Action: "rearm", AuthorizingMessage: "message"}); e == nil {
		t.Fatal("daemon restart reset durable budget")
	}
}

func TestFailedCollectionRemainsOwnedByNextDrive(t *testing.T) {
	c := completion{id: "missing", dispatch: "exact-token"}
	d := &Daemon{ctx: context.Background(), jobs: map[string]controlplane.JobSnapshot{}, pendingCompletion: &c}
	if err := d.driveRoundLoop(); err == nil {
		t.Fatal("expected failed collection")
	}
	if d.pendingCompletion == nil || d.pendingCompletion.dispatch != "exact-token" {
		t.Fatal("completion forgotten on generation failure")
	}
	d.driveStop.Store(true)
	if err := d.driveRoundLoop(); err != nil {
		t.Fatal(err)
	}
	if d.pendingCompletion == nil {
		t.Fatal("explicit stop consumed pending result")
	}
}

func TestIndependentDriveFailureKeepsRPCAndRequiresConsistentCheckpoint(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { stop(t, d) })
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.mu.Lock()
	d.jobs["broken"] = controlplane.JobSnapshot{ID: "broken", Payload: json.RawMessage(`{"wrong":true}`)}
	d.mu.Unlock()
	d.signal()
	h := awaitDrive(t, d, false, 1)
	if h.Consistent {
		t.Fatal("invalid plane reported consistent")
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "rearm", AuthorizingMessage: "message"}); e == nil {
		t.Fatal("rearmed inconsistent checkpoint")
	}
	// Removing the injected non-durable cache corruption restores consistency.
	d.mu.Lock()
	delete(d.jobs, "broken")
	d.mu.Unlock()
	if _, e = controllerCall(t, d, controller.Params{Action: "rearm", AuthorizingMessage: "message"}); e != nil {
		t.Fatal(e)
	}
	awaitDrive(t, d, true, 2)
}

func TestDriveGenerationPanicDoesNotEscapeSupervisor(t *testing.T) {
	// A missing child is deliberately invalid infrastructure and panics while
	// constructing the select. The supervisor receives an error, not a process exit.
	d := &Daemon{ctx: context.Background()}
	if e := d.runDriveGeneration(); e == nil || !strings.Contains(e.Error(), "drive panic") {
		t.Fatal(e)
	}
}

func TestDrivePersistReconcilesExactLostAcknowledgement(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { stop(t, d) })
	authorize(t, d)
	// Admit with the drive stopped so no effect can run during reconciliation.
	if _, e = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); e != nil {
		t.Fatal(e)
	}
	awaitDrive(t, d, false, 1)
	if _, e = call(t, d, "Admit", AdmitParams{ID: "lost-ack", Tree: "tree", AuthorizingMessage: "message"}, "lost-ack"); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	original := d.jobs["lost-ack"]
	d.mu.Unlock()
	var proposal envelope
	if e = decodeEnvelope(original.Payload, &proposal); e != nil {
		t.Fatal(e)
	}
	proposal.Admission.Priority++
	committed, e := d.persist(ctx, original, proposal, nil)
	if e != nil {
		t.Fatal(e)
	}
	// The caller retained the old version after an imaginary lost response.
	recovered, e := d.persist(ctx, original, proposal, nil)
	if e != nil || recovered.Version != committed.Version {
		t.Fatal(recovered.Version, committed.Version, e)
	}
	proposal.Admission.Priority++
	if _, e = d.persist(ctx, original, proposal, nil); e == nil {
		t.Fatal("different proposed checkpoint adopted")
	}
	actual, e := d.store.GetJob(ctx, original.ID)
	if e != nil || actual.Version != committed.Version {
		t.Fatal("conflict changed checkpoint", actual, e)
	}
}

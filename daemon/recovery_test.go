package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
)

// FAE-CP-006: revalidateCheckpoints refuses when the plane's event clock is
// behind its events, and completes an unchecked plane-recovery marker once an
// initial divergence is reconciled and checkpoint revalidation succeeds.
func TestRevalidateCheckpointsClockCheckAndRecoveryMarkerUpdate(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)

	if _, err = call(t, d, "Admit", AdmitParams{ID: "recover-task", Tree: "tree", AuthorizingMessage: "message"}, "recover-task"); err != nil {
		t.Fatal(err)
	}

	// Simulate an initial divergence during recordRecovery so plane-recovery is
	// recorded with Checked: false.
	if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "damage task row", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE tasks SET state='Escalated' WHERE task_id='recover-task'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pid := d.currentChild().PID()
	outage := planeOutage{Cause: "exited", Detected: time.Now().UTC().Add(-500 * time.Millisecond), PID: 1}
	if err = d.recordRecovery(outage, 1, pid); err != nil {
		t.Fatal(err)
	}
	raw, err := d.store.GetMarker(ctx, "plane-recovery")
	if err != nil {
		t.Fatal(err)
	}
	var record planeOutageRecord
	if err = json.Unmarshal(raw, &record); err != nil || record.Checked || len(record.Consistency.Diverged) != 1 {
		t.Fatalf("expected initial unchecked recovery with diverged task, got %+v (%v)", record, err)
	}

	// (a) Reconcile the task row, damage the event clock, and verify
	// revalidateCheckpoints refuses and keeps revalidate=true.
	if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "reconcile task row", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		var jobState string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE job_id='recover-task'`).Scan(&jobState); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE tasks SET state=? WHERE task_id='recover-task'`, jobState)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = d.planeDB.ExecContext(ctx, `UPDATE event_clock SET sequence=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	err = d.revalidateCheckpoints(ctx)
	if err == nil || !strings.Contains(err.Error(), "event clock (0) is behind its last event") {
		t.Fatalf("expected revalidateCheckpoints to refuse event clock behind LastEvent, got %v", err)
	}
	if !d.revalidate.Load() {
		t.Fatal("revalidateCheckpoints cleared revalidate after clock refusal")
	}

	// (b) Restore event_clock and verify revalidateCheckpoints succeeds and
	// updates plane-recovery with Checked: true.
	var savedClock int64
	if err = d.planeDB.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM events`).Scan(&savedClock); err != nil {
		t.Fatal(err)
	}
	if _, err = d.planeDB.ExecContext(ctx, `UPDATE event_clock SET sequence=? WHERE id=1`, savedClock); err != nil {
		t.Fatal(err)
	}
	if err = d.revalidateCheckpoints(ctx); err != nil {
		t.Fatalf("revalidateCheckpoints after reconciliation: %v", err)
	}
	if d.revalidate.Load() {
		t.Fatal("revalidate still set after clean revalidateCheckpoints")
	}
	raw, err = d.store.GetMarker(ctx, "plane-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &record); err != nil || !record.Checked || !record.Consistency.Consistent() {
		t.Fatalf("expected plane-recovery Checked=true and consistent after revalidateCheckpoints, got %+v (%v)", record, err)
	}
}

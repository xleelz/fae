package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
)

// awaitRecovery waits for a checked plane-recovery record newer than after.
func awaitRecovery(t *testing.T, d *Daemon, after time.Time) planeOutageRecord {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := d.store.GetMarker(context.Background(), "plane-recovery")
		var r planeOutageRecord
		if err == nil && json.Unmarshal(raw, &r) == nil && r.Recovered.After(after) && r.Checked {
			return r
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("plane never recovered")
	return planeOutageRecord{}
}

// assertTransitionChain checks a finished task's transition log: one row per
// recorded state change, each starting where the previous ended, from Pending
// to Done, so an outage neither duplicated nor invented a transition.
func assertTransitionChain(t *testing.T, d *Daemon, id string) {
	t.Helper()
	snapshot := awaitState(t, d, id, "Done")
	var e envelope
	if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, ev := range e.Job.History {
		if ev.From != "" && ev.To != "" && (ev.Kind == "transition" || ev.Kind == "migrated" && ev.From != ev.To) {
			want++
		}
	}
	rows, err := d.store.TaskTransitions(context.Background(), id, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != want || len(rows) == 0 || rows[0].From != "Pending" || rows[len(rows)-1].To != "Done" {
		t.Fatalf("task %s: %d transition rows for %d recorded changes", id, len(rows), want)
	}
	for n := 1; n < len(rows); n++ {
		if rows[n].From != rows[n-1].To {
			t.Fatalf("task %s transition %d starts at %s after %s", id, n, rows[n].From, rows[n-1].To)
		}
	}
}

// FAE-CP-006 acceptance: the Dolt child is killed mid-drive; the daemon
// restarts it, re-validates, records the outage and resumes, and the
// transition log has no duplicate or invented row.
func TestDoltKilledMidDriveRecoversWithoutDuplicateTransitions(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	tasks := []string{"outage-a", "outage-b", "outage-c"}
	for _, id := range tasks {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	// Kill once the drive is part-way through the first task.
	deadline := time.Now().Add(60 * time.Second)
	for {
		rows, err := d.store.TaskTransitions(ctx, "outage-a", 0, 100)
		if err == nil && len(rows) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drive made no progress", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	killed := time.Now()
	pid := d.currentChild().PID()
	if err = syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	record := awaitRecovery(t, d, killed)
	if record.Cause != "exited" || record.PID != pid || record.NewPID == pid || !record.Consistency.Consistent() || record.Attempts < 1 {
		t.Fatalf("outage record %+v", record)
	}
	for _, id := range tasks {
		assertTransitionChain(t, d, id)
	}
	if n := countRows(t, d, fmt.Sprintf(`SELECT COUNT(*) FROM audit WHERE action='Dolt recovered' AND outcome='ok' AND inputs LIKE '%%"pid":%d%%'`, pid)); n != 1 {
		t.Fatal("outage not audited", n)
	}
}

// FAE-CP-006: a hung Dolt (alive, not answering) is detected by a failed
// ping under a deadline, killed and restarted, and the work completes.
func TestHungDoltDetectedAndRestarted(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "hung", Tree: "tree", AuthorizingMessage: "message"}, "hung"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		rows, err := d.store.TaskTransitions(ctx, "hung", 0, 100)
		if err == nil && len(rows) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drive made no progress", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopped := time.Now()
	pid := d.currentChild().PID()
	if err = syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	record := awaitRecovery(t, d, stopped)
	if record.Cause != "unresponsive" || record.PID != pid || record.NewPID == pid || !record.Consistency.Consistent() {
		t.Fatalf("outage record %+v", record)
	}
	if detected := record.Detected.Sub(stopped); detected > 3*(planeProbePeriod+planeProbeDeadline) {
		t.Fatalf("hung plane detected after %s", detected)
	}
	assertTransitionChain(t, d, "hung")
	var c controlplane.Consistency
	if c, err = d.store.CheckConsistency(ctx); err != nil || !c.Consistent() {
		t.Fatal(c, err)
	}
}

// FAE-CP-006: after a plane restart the outage and the recovery are recorded
// before the consistency check, which gets the start path's budget (a bound
// per read, none overall), and a check that fails still makes the drive
// re-validate its cached checkpoints before acting. Before, one 10 s context
// covered the whole check, and when it failed or ran out the outage was
// never recorded and the re-validation never requested: the drive
// generation ended and was re-armed as if nothing had happened.
func TestPlaneRecoveryIsRecordedBeforeTheCheckAndAFailedCheckStillRevalidates(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	rename := func(from, to string) {
		t.Helper()
		if err := d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "rename " + from, Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
			_, e := tx.ExecContext(ctx, `RENAME TABLE `+from+` TO `+to)
			return e
		}); err != nil {
			t.Fatal(err)
		}
	}
	// With the task rows out of reach the check fails at its first page.
	rename("tasks", "tasks_detached")
	pid := d.currentChild().PID()
	outage := planeOutage{Cause: "exited", Detected: time.Now().UTC().Add(-time.Second), PID: 1}
	if err = d.recordRecovery(outage, 1, pid); err == nil {
		t.Fatal("a failed consistency check was not reported")
	}
	raw, err := d.store.GetMarker(ctx, "plane-recovery")
	if err != nil {
		t.Fatal("the outage was not recorded before the check:", err)
	}
	var record planeOutageRecord
	if err = json.Unmarshal(raw, &record); err != nil || record.Cause != "exited" || record.PID != 1 || record.NewPID != pid || record.Attempts != 1 || record.Checked || record.OutageMS < 1000 {
		t.Fatalf("outage record %+v %v", record, err)
	}
	if !d.revalidate.Load() {
		t.Fatal("a failed check after a plane restart skipped the drive's re-validation")
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM audit WHERE action='Dolt recovered' AND outcome='unchecked'`); n != 1 {
		t.Fatal("unchecked recovery not audited", n)
	}
	// Once the plane is whole again the check passes and completes the record.
	rename("tasks_detached", "tasks")
	if err = d.recordRecovery(outage, 2, pid); err != nil {
		t.Fatal(err)
	}
	if raw, err = d.store.GetMarker(ctx, "plane-recovery"); err != nil || json.Unmarshal(raw, &record) != nil || !record.Checked || !record.Consistency.Consistent() || record.Attempts != 2 {
		t.Fatalf("completed record %+v %v", record, err)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM audit WHERE action='Dolt recovered' AND outcome='ok'`); n != 1 {
		t.Fatal("checked recovery not audited", n)
	}
}

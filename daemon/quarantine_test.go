package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// corruptStoredJob rewrites one durable row behind the daemon and returns the
// original snapshot.
func corruptStoredJob(t *testing.T, d *Daemon, id string, mutate func(*envelope)) controlplane.JobSnapshot {
	t.Helper()
	ctx := context.Background()
	original, err := d.store.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err = json.Unmarshal(original.Payload, &e); err != nil {
		t.Fatal(err)
	}
	mutate(&e)
	changed := original
	changed.Payload, _ = json.Marshal(e)
	changed.Events = nil
	if _, err = d.store.SaveJob(ctx, controlplane.Audit{Actor: "test", Action: "corrupt durable row", Outcome: "ok"}, changed, original.Version); err != nil {
		t.Fatal(err)
	}
	return original
}

// admitHeld admits tasks while the drive is stopped, so they stay Pending.
func admitHeld(t *testing.T, d *Daemon, ids ...string) {
	t.Helper()
	awaitDrive(t, d, true, 1)
	if _, err := controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	awaitDrive(t, d, false, 1)
	for _, id := range ids {
		if _, err := call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
}

func inspectQuarantine(t *testing.T, d *Daemon) QuarantineReport {
	t.Helper()
	raw, err := call(t, d, "QuarantineInspect", QuarantineParams{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var r QuarantineReport
	if err = json.Unmarshal(raw.Result, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func quarantineEntry(r QuarantineReport, id string) *QuarantinedTask {
	for i := range r.Quarantined {
		if r.Quarantined[i].ID == id {
			return &r.Quarantined[i]
		}
	}
	return nil
}

func countRows(t *testing.T, d *Daemon, query string) int {
	t.Helper()
	n := 0
	err := d.store.Mutate(context.Background(), controlplane.Audit{Actor: "test", Action: "inspect rows", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, query).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// FAE-ROB-042, FAE-ROB-001: a row quarantined at startup stays resolvable by
// audited operator verbs: inspect, release after repair, and retire.
func TestQuarantineOperatorVerbs(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	admitHeld(t, d, "repairable", "doomed")
	original := corruptStoredJob(t, d, "repairable", func(e *envelope) { e.Job.WorkflowHash = "corrupted" })
	corruptStoredJob(t, d, "doomed", func(e *envelope) { e.Job.WorkflowHash = "corrupted" })
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal("an invalid stored row refused the instance", err)
	}
	report := inspectQuarantine(t, d)
	for _, id := range []string{"repairable", "doomed"} {
		q := quarantineEntry(report, id)
		if q == nil || q.Reason == "" || q.Revalidation == "ok" || q.Row == nil || q.Row.State != "Pending" {
			t.Fatalf("quarantined row not inspectable: %+v", q)
		}
	}
	alerts := countRows(t, d, `SELECT COUNT(*) FROM events WHERE kind='task-quarantined'`)
	if alerts != 2 {
		t.Fatal("quarantine alerts", alerts)
	}
	// Refusals: no authority, unknown task, unrepaired row, blank reason.
	refused := func(method string, p QuarantineParams, want string) {
		t.Helper()
		if _, err := call(t, d, method, p, ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s %+v not refused with %q: %v", method, p, want, err)
		}
	}
	refused("QuarantineRelease", QuarantineParams{ID: "repairable"}, "message")
	refused("QuarantineRelease", QuarantineParams{ID: "absent", AuthorizingMessage: "message"}, "not quarantined")
	refused("QuarantineRelease", QuarantineParams{ID: "repairable", AuthorizingMessage: "message"}, "workflow")
	refused("QuarantineRetire", QuarantineParams{ID: "doomed", AuthorizingMessage: "message"}, "reason")
	if q := quarantineEntry(inspectQuarantine(t, d), "repairable"); q == nil {
		t.Fatal("refused release dropped the quarantine")
	}
	// Repair restores the exact original row; release returns it to the drive.
	current, err := d.store.GetJob(ctx, "repairable")
	if err != nil {
		t.Fatal(err)
	}
	repaired := current
	repaired.Payload, repaired.Events = original.Payload, nil
	if _, err = d.store.SaveJob(ctx, controlplane.Audit{Actor: "test", Action: "operator repair", Outcome: "ok"}, repaired, current.Version); err != nil {
		t.Fatal(err)
	}
	if q := quarantineEntry(inspectQuarantine(t, d), "repairable"); q == nil || q.Revalidation != "ok" {
		t.Fatalf("repair not visible to inspect: %+v", q)
	}
	if _, err = call(t, d, "QuarantineRelease", QuarantineParams{ID: "repairable", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "repairable", "Done")
	if _, err = call(t, d, "QuarantineRetire", QuarantineParams{ID: "doomed", Reason: "obsolete: corrupted in test", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	report = inspectQuarantine(t, d)
	if quarantineEntry(report, "doomed") != nil || len(report.Retired) != 1 || report.Retired[0].ID != "doomed" || report.Retired[0].Reason != "obsolete: corrupted in test" {
		t.Fatalf("retired row not reported: %+v", report)
	}
	refused("QuarantineRelease", QuarantineParams{ID: "doomed", AuthorizingMessage: "message"}, "not quarantined")
	if n := countRows(t, d, `SELECT COUNT(*) FROM audit WHERE action IN ('QuarantineRelease','QuarantineRetire') AND message_id='message'`); n != 2 {
		t.Fatal("quarantine verbs not audited", n)
	}
	// A retired row stays out of the drive across restarts without a new alert.
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	report = inspectQuarantine(t, d)
	if len(report.Quarantined) != 0 || len(report.Retired) != 1 {
		t.Fatalf("retired row re-quarantined: %+v", report)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM events WHERE kind='task-quarantined'`); n != alerts {
		t.Fatal("retired row alerted again", n)
	}
	d.mu.Lock()
	_, cached := d.jobs["doomed"]
	d.mu.Unlock()
	if cached {
		t.Fatal("retired row returned to the drive")
	}
}

// FAE-ROB-042: a stored row whose execution kind or disposition this daemon
// cannot honor fails closed for that row only, naming the context; the
// instance starts and drives the others.
func TestStartQuarantinesExecutorKindMismatch(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	admitHeld(t, d, "foreign", "native", "undisposed")
	corruptStoredJob(t, d, "foreign", func(e *envelope) { e.Job.ExecutionKind = "live" })
	corruptStoredJob(t, d, "undisposed", func(e *envelope) { e.Job.Disposition.Live = true })
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal("one row's execution kind refused the instance", err)
	}
	report := inspectQuarantine(t, d)
	q := quarantineEntry(report, "foreign")
	if q == nil || !strings.Contains(q.Reason, "executor kind live") || !strings.Contains(q.Revalidation, "executor kind live") {
		t.Fatalf("mismatch not quarantined with its context: %+v", q)
	}
	if q = quarantineEntry(report, "undisposed"); q == nil || !strings.Contains(q.Reason, "disposition") {
		t.Fatalf("invalid disposition not quarantined with its context: %+v", q)
	}
	awaitState(t, d, "native", "Done")
	if _, err = call(t, d, "QuarantineRelease", QuarantineParams{ID: "foreign", AuthorizingMessage: "message"}, ""); err == nil || !strings.Contains(err.Error(), "executor kind") {
		t.Fatal("mismatched row released", err)
	}
}

// FAE-ROB-001/-042: a task quarantined at run time while its dispatch runs
// cannot be released until that worker's completion is dropped; release then
// recovers the dropped dispatch without charge (scripted replays it).
func TestQuarantineReleaseAfterRuntimeIsolation(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"work": {{Output: json.RawMessage(`{"ok":true}`)}}, "slow/work": {{Output: json.RawMessage(`{"ok":true}`), Delay: 2 * time.Second}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := shortWorkflow()
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "slow", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "slow"); err != nil {
		t.Fatal(err)
	}
	dispatch := awaitInFlight(t, d, "slow", "work")
	// An undecodable cached row is isolated by the round's decode check while
	// its worker still runs; the durable row stays valid.
	d.mu.Lock()
	snapshot := d.jobs["slow"]
	snapshot.Payload = json.RawMessage(`{"wrong":true}`)
	d.jobs["slow"] = snapshot
	d.mu.Unlock()
	d.signal()
	running := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		d.mu.Lock()
		reason, active := d.quarantined["slow"], d.active["slow"]
		d.mu.Unlock()
		if reason != "" {
			running = active
			break
		}
	}
	if !running {
		t.Fatal("runtime quarantine lost its running dispatch")
	}
	if _, err = call(t, d, "QuarantineRelease", QuarantineParams{ID: "slow", AuthorizingMessage: "message"}, ""); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatal("release relaunched a running dispatch", err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		d.mu.Lock()
		active := d.active["slow"]
		d.mu.Unlock()
		if !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stale completion never retired the quarantined dispatch")
		}
	}
	if _, err = call(t, d, "QuarantineRelease", QuarantineParams{ID: "slow", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	done := awaitState(t, d, "slow", "Done")
	var e envelope
	if err = json.Unmarshal(done.Payload, &e); err != nil {
		t.Fatal(err)
	}
	if e.Job.Failures["dispatch-recovery"] != 0 || e.Job.Failures["crash-recovery"] != 0 {
		t.Fatal("isolated dispatch charged on release", e.Job.Failures)
	}
	replayed := false
	for _, p := range e.Job.Provenance {
		replayed = replayed || p.DispatchID == dispatch
	}
	if !replayed {
		t.Fatal("dropped dispatch was not replayed by its exact token")
	}
}

// FAE-ROB-042: a row lacking its submission disposition is never driven as
// Local: it fails closed with a message naming the missing context, stays
// visible, and remains resolvable by an operator verb.
func TestStartFailsClosedOnMissingDisposition(t *testing.T) {
	for bits := 0; bits < 8; bits++ {
		if CanDriveDisposition(bits&1 != 0, bits&2 != 0, bits&4 != 0) != (bits != 0) {
			t.Fatal("disposition floor", bits)
		}
	}
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	admitHeld(t, d, "legacy")
	corruptStoredJob(t, d, "legacy", func(e *envelope) { e.Job.Disposition = domain.Disposition{} })
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal("a row without a disposition refused the instance", err)
	}
	q := quarantineEntry(inspectQuarantine(t, d), "legacy")
	for _, context := range []string{"disposition", "mode", "dry-run or live", "ticket room", "review destination branch", "push namespace"} {
		if q == nil || !strings.Contains(q.Reason, context) || !strings.Contains(q.Revalidation, context) {
			t.Fatalf("missing disposition not named (%s): %+v", context, q)
		}
	}
	if _, err = call(t, d, "QuarantineRelease", QuarantineParams{ID: "legacy", AuthorizingMessage: "message"}, ""); err == nil || !strings.Contains(err.Error(), "disposition") {
		t.Fatal("a row without a disposition was released to the drive", err)
	}
	if _, err = call(t, d, "QuarantineRetire", QuarantineParams{ID: "legacy", Reason: "obsolete: legacy row without disposition", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal("row not resolvable by an operator verb", err)
	}
	if s, err := d.store.GetJob(ctx, "legacy"); err != nil || s.State != "Pending" {
		t.Fatal("fail-closed row changed or was driven", s.State, err)
	}
}

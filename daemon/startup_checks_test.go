package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
)

// startupAuditCount counts the audit rows of one action whose inputs hold text.
func startupAuditCount(t *testing.T, ctx context.Context, d *Daemon, action, text string) int {
	t.Helper()
	var n int
	if err := d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "count startup audits", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor='daemon' AND action=? AND inputs LIKE ?`, action, "%"+text+"%").Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// FAE-WF-093: every stored workflow revision is checked at start, and the
// binary refuses to start, naming the revision, when it cannot read one
// (workflow.Load; operator ruling, 2026-10-30). Before (wave 11), the
// start named it in a daemon line and an alert and went on. Now the refusal
// names the workflow, revision, hash and error, what uses it and how to
// recover; the daemon line says the same, and the loadable revisions, the
// shipped default's among them, are not named.
func TestStartRefusesAStoredRevisionItCannotLoad(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if checked, _ := lookup(statusBody(t, d, nil), "startup_checks.revisions.checked"); checked.(float64) < 1 {
		t.Fatal("the shipped default's revision was not checked", checked)
	}
	// A revision another binary wrote that this one cannot load (a later
	// schema, a field it does not know): the write path's own validation is
	// bypassed to plant it.
	broken := json.RawMessage(`{"schema_version":3,"id":"broken","start":"nowhere","from_a_later_binary":true}`)
	rev, err := d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "plant a revision this binary cannot load", Outcome: "ok"}, "broken", "broken", broken, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	if d, err = Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("a stored revision this binary cannot load did not refuse the start")
	}
	for _, want := range []string{
		"refusing to start: 1 stored workflow revision(s) cannot be loaded by this binary (FAE-WF-093)",
		"workflow broken revision 1 (" + rev.Hash + "): invalid workflow",
		"from_a_later_binary: unknown field",
		"schema_version: must be 1 or 2",
		"no active task is pinned to it; it is the workflow's latest revision, which new admissions bind",
		"start the binary that wrote the revision again",
		"restore the control plane from a backup",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "workflow default") {
		t.Error("the refusal names the loadable default", err)
	}
	layout, _ := Paths(cfg.Root, cfg.Instance)
	log, err := os.ReadFile(layout.Log)
	if err != nil || !strings.Contains(string(log), "stored workflow broken revision 1 ("+rev.Hash+") cannot be loaded by this binary, so the start refuses") {
		t.Fatalf("no daemon line names the revision: %v", err)
	}
}

// FAE-WF-093: the start checks every stored revision, however many there
// are. Before, the check listed at most 10,000 revisions (the listing failed
// past that) and parsed them within 30 s, and a revision it never reached
// let the start go on unchecked. Now it reads them a page at a time, each
// read bounded, so a revision this binary cannot load past 10,001 loadable
// ones still refuses the start.
func TestStartChecksEveryStoredRevisionPastTheListingBound(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "store many revisions of one loadable content", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		var digest string
		if err := tx.QueryRowContext(ctx, `SELECT content_digest FROM workflows WHERE workflow_id='default' ORDER BY revision LIMIT 1`).Scan(&digest); err != nil {
			return err
		}
		for first := 1; first <= 10001; first += 1000 {
			var rows []string
			for n := first; n < first+1000 && n <= 10001; n++ {
				rows = append(rows, fmt.Sprintf("('bulk',%d,NULL,'bulk','%s','test',NOW(6))", n, digest))
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO workflows (workflow_id,revision,parent_revision,name,content_digest,created_by,created_at) VALUES `+strings.Join(rows, ",")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Ordered after every bulk and default revision.
	if _, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "plant a revision this binary cannot load", Outcome: "ok"}, "zz-broken", "broken", json.RawMessage(`{"schema_version":3,"id":"zz-broken","start":"nowhere","from_a_later_binary":true}`), func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	if d, err = Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("a revision past 10,001 stored revisions went unchecked: the start was not refused")
	}
	if !strings.Contains(err.Error(), "workflow zz-broken revision 1") || strings.Contains(err.Error(), "workflow bulk") {
		t.Fatalf("the refusal does not name exactly the unloadable revision: %v", err)
	}
}

// FAE-CP-006: the start checks the control plane's consistency before it
// loads any task, as a restarted plane is checked. Before, startup never ran
// the check. A task whose relational row no longer records its snapshot's
// state is quarantined at start, not loaded and driven, and named in
// startup_checks with the plane's consistency.
func TestStartChecksPlaneConsistencyBeforeLoadingTasks(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	for _, id := range []string{"diverged", "intact"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "damage a task row", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `UPDATE tasks SET state='Escalated' WHERE task_id='diverged'`)
		if n, _ := r.RowsAffected(); err == nil && n != 1 {
			t.Errorf("%d task rows damaged", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal("a diverged task row refused the start:", err)
	}
	d.mu.Lock()
	reason, isolated := d.quarantined["diverged"]
	_, loaded := d.jobs["diverged"]
	_, intact := d.jobs["intact"]
	d.mu.Unlock()
	if !isolated || loaded || !intact || !strings.Contains(reason, "task rows diverged from the durable snapshot") {
		t.Fatalf("diverged task: quarantined %t (%q), loaded %t; intact task loaded %t", isolated, reason, loaded, intact)
	}
	body := statusBody(t, d, nil)
	if consistent, _ := lookup(body, "startup_checks.plane.consistent"); consistent != false {
		t.Fatalf("startup_checks %v", body["startup_checks"])
	}
	if quarantined, _ := lookup(body, "startup_checks.plane.quarantined"); len(quarantined.([]any)) != 1 || quarantined.([]any)[0] != "diverged" {
		t.Fatalf("startup_checks %v", body["startup_checks"])
	}
	stop(t, d)
}

// FAE-CP-006: a plane whose event clock is behind its own events is not
// started on: every later event write depends on that clock. Before, the
// start ran no consistency check and came up on it.
func TestStartRefusesAPlaneWhoseEventClockIsBehind(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "damage the event clock", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE event_clock SET sequence=0 WHERE id=1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	if d, err = Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("a plane whose event clock is behind its events was started on")
	}
	if !strings.Contains(err.Error(), "event clock (0) is behind its last event") {
		t.Fatalf("the refusal does not name the damage: %v", err)
	}
}

// FAE-CP-006: the start checks the plane's event clock before it writes any
// event. Before, the start audited the empty process records it removed (an
// event insert) before the consistency check, so on a plane whose clock is
// behind its events it failed on that insert's duplicate sequence instead of
// naming the damage. Now the refusal names the damaged clock.
func TestStartNamesADamagedEventClockBeforeWritingAnEvent(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "damage the event clock", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE event_clock SET sequence=0 WHERE id=1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	layout, err := Paths(cfg.Root, cfg.Instance)
	if err != nil {
		t.Fatal(err)
	}
	// An empty record an earlier process left: its removal is audited with
	// an event as soon as the plane is up.
	record := filepath.Join(layout.Run, "plane-processes", "process-empty.json")
	if err = os.MkdirAll(filepath.Dir(record), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(record, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(record, beforeThisTestProcess, beforeThisTestProcess); err != nil {
		t.Fatal(err)
	}
	if d, err = Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("a plane whose event clock is behind its events was started on")
	}
	if !strings.Contains(err.Error(), "event clock (0) is behind its last event") {
		t.Fatalf("the refusal does not name the damaged clock: %v", err)
	}
}

// FAE-CP-006: the start compares a task's backlog row with its snapshot as
// well as its task row, and `fae quarantine release` repairs the diverged
// rows from the snapshot. Before, a diverged backlog row was not compared, so
// the task loaded; and release re-validated only the snapshot, so a task
// quarantined for a diverged task row was quarantined again at every start
// while inspect called it releasable. Now both are quarantined, inspect
// names the divergence, release sets the rows to the snapshot's state in an
// audited write, and the next start loads both. A task whose task row is
// missing is not repaired: release refuses it, naming the remedies.
func TestQuarantineReleaseRepairsRowsTheStartFoundDiverged(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	holdDrive := func(generation uint64) {
		awaitDrive(t, d, true, generation)
		d.driveStop.Store(true)
		d.signal()
		awaitDrive(t, d, false, generation)
	}
	holdDrive(1)
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{{AdmitParams: AdmitParams{ID: "backlogged", Tree: "tree", Priority: 10, Content: "x"}}}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"tasked", "rowless"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "damage task and backlog rows", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		for _, damage := range []string{`UPDATE backlog_items SET state='Escalated' WHERE item_id='backlogged'`, `UPDATE tasks SET state='Escalated' WHERE task_id='tasked'`, `DELETE FROM tasks WHERE task_id='rowless'`} {
			if _, err := tx.ExecContext(ctx, damage); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	holdDrive(1)
	d.mu.Lock()
	_, backlogged := d.quarantined["backlogged"]
	_, tasked := d.quarantined["tasked"]
	_, rowless := d.quarantined["rowless"]
	d.mu.Unlock()
	if !backlogged || !tasked || !rowless {
		t.Fatalf("diverged rows quarantined at start: backlog %t, task %t, missing task row %t", backlogged, tasked, rowless)
	}
	raw, err := call(t, d, "QuarantineInspect", QuarantineParams{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Quarantined []map[string]any `json:"quarantined"`
	}
	if err = json.Unmarshal(raw.Result, &report); err != nil {
		t.Fatal(err)
	}
	divergence := map[string]string{}
	for _, q := range report.Quarantined {
		divergence[q["id"].(string)], _ = q["row_divergence"].(string)
	}
	for id, want := range map[string]string{"backlogged": "backlog row state Escalated differ from the snapshot's state", "tasked": "task row state Escalated differ from the snapshot's state", "rowless": "the task row is missing; release refuses"} {
		if !strings.Contains(divergence[id], want) {
			t.Errorf("inspect of %s says %q, want %q", id, divergence[id], want)
		}
	}
	if _, err = call(t, d, "QuarantineRelease", QuarantineParams{ID: "rowless", AuthorizingMessage: "message"}, ""); err == nil || !strings.Contains(err.Error(), "the task row is missing; retire it") {
		t.Fatalf("release of a task without a task row: %v", err)
	}
	for _, id := range []string{"backlogged", "tasked"} {
		raw, err = call(t, d, "QuarantineRelease", QuarantineParams{ID: id, AuthorizingMessage: "message"}, "")
		if err != nil {
			t.Fatal(err)
		}
		var released map[string]any
		_ = json.Unmarshal(raw.Result, &released)
		if repaired, _ := released["repaired"].(string); !strings.Contains(repaired, "state Escalated differ from the snapshot's state") {
			t.Fatalf("release of %s does not say what it repaired: %v", id, released)
		}
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM jobs j JOIN tasks t ON t.task_id=j.job_id LEFT JOIN backlog_items b ON b.item_id=j.job_id WHERE j.job_id IN ('backlogged','tasked') AND t.state=j.state AND (b.item_id IS NULL OR b.state=j.state)`); n != 2 {
		t.Fatalf("%d of the 2 released tasks have rows matching their snapshots", n)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM audit WHERE action='quarantine-release-repair' AND outcome='repaired' AND message_id='message'`); n != 2 {
		t.Fatalf("%d audited repairs, want 2", n)
	}
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	d.mu.Lock()
	_, backlogged = d.quarantined["backlogged"]
	_, tasked = d.quarantined["tasked"]
	_, rowless = d.quarantined["rowless"]
	d.mu.Unlock()
	if backlogged || tasked || !rowless {
		t.Fatalf("after release the next start quarantined: backlog %t, task %t; the unreleased task %t", backlogged, tasked, rowless)
	}
}

// FAE-CP-006: the plane's consistency check reads every active task, a page
// of diverged tasks at a time. Before, its one query stopped at 1,000
// diverged tasks, so the tasks past it loaded unchecked.
func TestCheckConsistencyFindsEveryDivergedTask(t *testing.T) {
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
	if _, err = call(t, d, "Admit", AdmitParams{ID: "real", Tree: "tree", AuthorizingMessage: "message"}, "real"); err != nil {
		t.Fatal(err)
	}
	const planted = 1005
	mutate := func(action string, body func(context.Context, *sql.Tx) error) {
		t.Helper()
		if err := d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: action, Outcome: "ok"}, body); err != nil {
			t.Fatal(err)
		}
	}
	// Active bound jobs without task rows: each one diverged.
	mutate("plant diverged jobs", func(ctx context.Context, tx *sql.Tx) error {
		var molecule, digest string
		if err := tx.QueryRowContext(ctx, `SELECT molecule_id,payload_digest FROM jobs WHERE job_id='real'`).Scan(&molecule, &digest); err != nil {
			return err
		}
		var rows []string
		for n := 0; n < planted; n++ {
			rows = append(rows, fmt.Sprintf("('planted-%04d',1,'default',1,'target',1,'Running','%s',1,'%s')", n, digest, molecule))
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO jobs (job_id,version,workflow_id,workflow_revision,target_id,target_revision,state,payload_digest,payload_size,molecule_id) VALUES `+strings.Join(rows, ","))
		return err
	})
	c, err := d.store.CheckConsistency(ctx)
	mutate("remove the planted jobs", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE job_id LIKE 'planted-%'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Diverged) != planted || c.Diverged[0] != "planted-0000" || c.Diverged[planted-1] != fmt.Sprintf("planted-%04d", planted-1) {
		t.Fatalf("%d diverged tasks found of %d", len(c.Diverged), planted)
	}
}

// FAE-WF-093, FAE-ROB-015: a refused start on an older schema (v1) leaves the
// older schema version and its audit/alerts/events row counts 100% unmutated,
// while recording the refusal alert in <root>/alerts.log on disk.
func TestStartRefusesAnUnrunnableRevisionBeforeMigratingAnOlderSchema(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	legacy := unrunnableLegacyDocument("legacy")
	raw, _ := json.Marshal(legacy)
	if _, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "older binary install", Outcome: "ok"}, "legacy", "legacy", raw, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	plantPinnedTask(t, d, cfg, "pinned-v1", "legacy", 1, legacy)
	if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "simulate schema 1 plane", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM migrations WHERE version > 1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stop(t, d)

	layout, err := Paths(cfg.Root, cfg.Instance)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(layout.Root, alertsFileName))
	binary := cfg.DoltBinary
	if binary == "" {
		binary = "dolt"
	}
	sqlOut := func(query string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, "sql", "-r", "csv", "-q", query)
		cmd.Dir = layout.Data
		cmd.Env = scopedEnv(layout.Root)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("dolt sql %q: %v: %s", query, err, out)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		return strings.TrimSpace(lines[len(lines)-1])
	}
	beforeAudit := sqlOut(`SELECT COUNT(*) FROM audit`)
	beforeAlerts := sqlOut(`SELECT COUNT(*) FROM alerts`)
	beforeEvents := sqlOut(`SELECT COUNT(*) FROM events`)

	if d, err = Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("expected start refusal on unrunnable revision in v1 schema")
	}
	if got := sqlOut(`SELECT COALESCE(MAX(version),0) FROM migrations`); got != "1" {
		t.Fatalf("refused start migrated schema from 1 to %s", got)
	}
	if got := sqlOut(`SELECT COUNT(*) FROM audit`); got != beforeAudit {
		t.Fatalf("audit row count changed on v1 database: got %s, want %s", got, beforeAudit)
	}
	if got := sqlOut(`SELECT COUNT(*) FROM alerts`); got != beforeAlerts {
		t.Fatalf("alerts row count changed on v1 database: got %s, want %s", got, beforeAlerts)
	}
	if got := sqlOut(`SELECT COUNT(*) FROM events`); got != beforeEvents {
		t.Fatalf("events row count changed on v1 database: got %s, want %s", got, beforeEvents)
	}
	alertBytes, err := os.ReadFile(filepath.Join(layout.Root, alertsFileName))
	if err != nil || !strings.Contains(string(alertBytes), `"workflow_id":"legacy"`) {
		t.Fatalf("expected refusal alert in alerts.log, got %q (%v)", string(alertBytes), err)
	}
}

// FAE-ARC-001, FAE-ROB-014: starting a second Daemon on the same Root while
// the first is live fails naming the live owner PID; starting after a dead PID
// left in the lock file reclaims the lock and records owner-reclaimed.
func TestInstanceLockLiveRefusalAndStalePIDReclamation(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	livePID := os.Getpid()
	if d2, err := Start(ctx, cfg); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("pid %d", livePID)) {
		if d2 != nil {
			stop(t, d2)
		}
		stop(t, d)
		t.Fatalf("expected live instance lock refusal naming pid %d, got %v", livePID, err)
	}
	stop(t, d)

	// Spawn a short-lived child process that exits so we have a guaranteed dead PID.
	child := exec.Command("true")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	deadPID := child.Process.Pid
	_ = child.Wait()

	layout, err := Paths(cfg.Root, cfg.Instance)
	if err != nil {
		t.Fatal(err)
	}
	staleOwner, _ := json.Marshal(owner{PID: deadPID, Start: "Thu Jan  1 00:00:00 1970", Launched: time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(layout.Run, "lock"), staleOwner, 0600); err != nil {
		t.Fatal(err)
	}

	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start failed to reclaim stale lock for dead pid %d: %v", deadPID, err)
	}
	defer stop(t, d)
	markerRaw, err := d.store.GetMarker(ctx, "owner-reclaimed")
	if err != nil || strings.TrimSpace(string(markerRaw)) != "true" {
		t.Fatalf("expected owner-reclaimed marker, got %q (%v)", string(markerRaw), err)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM audit WHERE actor='daemon' AND action='reclaim dead owner' AND outcome='ok'`); n != 1 {
		t.Fatalf("expected 1 reclaim dead owner audit row, got %d", n)
	}
}

// FAE-ARC-021: Start with cfg.Workspace not matching the pinned workspace
// refuses before creating any files in the mismatched workspace or creating
// the plane directory if it does not exist yet.
func TestWorkspaceMismatchRefusesBeforeTouchingWorkspaceOrPlane(t *testing.T) {
	cfg, ctx := fixture(t)
	layout, err := Paths(cfg.Root, cfg.Instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := mkdirPrivate(layout.Root); err != nil {
		t.Fatal(err)
	}
	pinned, _ := json.Marshal(map[string]string{"workspace": cfg.Workspace})
	if err := writePrivate(layout.Binding, pinned); err != nil {
		t.Fatal(err)
	}
	mismatched := filepath.Join(t.TempDir(), "other-workspace")
	if err := os.MkdirAll(mismatched, 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Workspace = mismatched
	if d, err := Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("expected workspace mismatch refusal")
	}
	entries, err := os.ReadDir(mismatched)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expected mismatched workspace untouched, got %d entries (%v)", len(entries), err)
	}
	if _, err := os.Stat(layout.Data); !os.IsNotExist(err) {
		t.Fatalf("expected plane directory %s not to be created on workspace mismatch, got err=%v", layout.Data, err)
	}
}

// FAE-CP-005: Start with cfg.DoltBinary == "" resolves dolt via PATH and starts cleanly.
func TestDoltPathResolutionWithEmptyBinaryConfig(t *testing.T) {
	cfg, ctx := fixture(t)
	if cfg.DoltBinary != "" {
		t.Setenv("PATH", filepath.Dir(cfg.DoltBinary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	cfg.DoltBinary = ""
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start with empty DoltBinary failed: %v", err)
	}
	stop(t, d)
}

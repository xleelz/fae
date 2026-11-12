package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-093 (operator decision, 2026-10-30): a stored revision this binary
// reads but cannot run (workflow.Parse fails) refuses the start while an
// active task is pinned to it, naming the workflow, revision, hash, failing
// rules, tasks and a recovery that needs no older binary; the refused start
// writes no revision and no task row. Under `serve
// --quarantine-unrunnable-revisions` the start proceeds and quarantines
// exactly those tasks, with an alert each and status listing them; release
// refuses while the revision stays unrunnable; `fae migrate` moves a task to
// a revision this binary validates and it runs; `fae quarantine retire`
// retires the other; the next start needs no flag, lists the revision with
// no task, and the flag is then a no-op. Before, the revision was listed as
// "invalid" and its tasks were loaded and driven on it.
func TestStartRefusesAnUnrunnableRevisionWithAnActiveTask(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	legacy := unrunnableLegacyDocument("legacy")
	raw, _ := json.Marshal(legacy)
	if _, err = workflow.Load(raw); err != nil || validWorkflow(raw) == nil {
		t.Fatal("fixture is not an unrunnable revision", err)
	}
	unrunnable, err := d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "older binary install", Outcome: "ok"}, "legacy", "legacy", raw, func([]byte) error { return nil })
	if err != nil || unrunnable.Number != 1 {
		t.Fatal(unrunnable, err)
	}
	valid := defaults.Shipped()
	valid.ID = "legacy"
	validRaw, _ := json.Marshal(valid)
	if _, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "valid revision", Outcome: "ok"}, "legacy", "legacy", validRaw, validWorkflow); err != nil {
		t.Fatal(err)
	}
	plantPinnedTask(t, d, cfg, "pinned", "legacy", 1, legacy)
	plantPinnedTask(t, d, cfg, "pinned-too", "legacy", 1, legacy)
	revisions := countRows(t, d, `SELECT COUNT(*) FROM workflows`)
	jobVersions := countRows(t, d, `SELECT SUM(version) FROM jobs`)
	stop(t, d)

	if d, err = Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("an unrunnable revision with active tasks pinned to it did not refuse the start")
	}
	for _, want := range []string{
		"refusing to start: 1 stored workflow revision(s) cannot be run by this binary and have active tasks pinned to them (FAE-WF-093)",
		"workflow legacy revision 1 (" + unrunnable.Hash + "): invalid workflow",
		"brief key must be default",
		"active tasks [pinned pinned-too] are pinned to it",
		"fae serve ... --quarantine-unrunnable-revisions",
		"fae migrate --task ID --revision N",
		"fae quarantine retire --task ID --reason TEXT",
		"Nothing was written",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "workflow default") || strings.Contains(err.Error(), "which new admissions bind") {
		t.Error("the refusal names the valid default or calls revision 1 the latest", err)
	}
	layout, _ := Paths(cfg.Root, cfg.Instance)
	if log, _ := os.ReadFile(layout.Log); !strings.Contains(string(log), "stored workflow legacy revision 1 ("+unrunnable.Hash+") loads but cannot be run by this binary and active tasks are pinned to it, so the start refuses (FAE-WF-093)") {
		t.Fatal("no daemon line names the refusal")
	}

	cfg.QuarantineUnrunnableRevisions = true
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal("the flag did not let the start proceed:", err)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM workflows`); n != revisions {
		t.Fatalf("the refused start, or the quarantining one, wrote a revision: %d rows, had %d", n, revisions)
	}
	if n := countRows(t, d, `SELECT SUM(version) FROM jobs`); n != jobVersions {
		t.Fatalf("a start wrote a task row: version sum %d, had %d", n, jobVersions)
	}
	d.mu.Lock()
	reason := d.quarantined["pinned"]
	_, loaded := d.jobs["pinned"]
	d.mu.Unlock()
	if loaded || !strings.HasPrefix(reason, "unrunnable-revision: workflow legacy revision 1 ("+unrunnable.Hash+") cannot be run by this binary (FAE-WF-093); migrate the task") || !strings.Contains(reason, "the failing rules: invalid workflow") {
		t.Fatalf("pinned task loaded %t, quarantined %q", loaded, reason)
	}
	body := statusBody(t, d, nil)
	if q, _ := lookup(body, "startup_checks.revisions.quarantined"); fmt.Sprint(q) != "[pinned pinned-too]" {
		t.Fatalf("startup_checks.revisions %v", body["startup_checks"])
	}
	if tasks, _ := lookup(body, "startup_checks.revisions.invalid"); fmt.Sprint(tasks.([]any)[0].(map[string]any)["tasks"]) != "[pinned pinned-too]" {
		t.Fatalf("startup_checks.revisions %v", body["startup_checks"])
	}
	if q, _ := lookup(body, "snapshot.quarantined.pinned-too"); q == nil {
		t.Fatalf("status does not list the quarantined task: %v", body["snapshot"])
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM events WHERE kind='task-quarantined' AND payload LIKE '%"task":"pinned"%'`); n != 1 {
		t.Fatalf("%d task-quarantined events for the task, want 1", n)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM audit WHERE actor='daemon' AND action='task-quarantined' AND outcome='alert'`); n != 2 {
		t.Fatalf("%d task-quarantined alerts, want one per task", n)
	}
	if n := startupAuditCount(t, ctx, d, "unrunnable-workflow-revision", `"quarantined":true`); n != 1 {
		t.Fatalf("%d unrunnable-workflow-revision alerts under the flag, want 1", n)
	}
	if n := startupAuditCount(t, ctx, d, "unrunnable-workflow-revision", `"quarantined":false`); n != 1 {
		t.Fatalf("%d unrunnable-workflow-revision alerts from the refused start, want 1", n)
	}
	report := inspectQuarantine(t, d)
	if len(report.Quarantined) != 2 || !strings.Contains(report.Quarantined[0].Revalidation, "cannot be run by this binary") {
		t.Fatalf("inspect %+v", report.Quarantined)
	}
	if _, err = call(t, d, "QuarantineRelease", QuarantineParams{ID: "pinned", AuthorizingMessage: "message"}, ""); err == nil || !strings.Contains(err.Error(), "quarantined task pinned cannot be released: workflow legacy revision 1") || !strings.Contains(err.Error(), "fae migrate") {
		t.Fatalf("release of a task on an unrunnable revision: %v", err)
	}
	if _, err = call(t, d, "Migrate", MigrateParams{ID: "pinned", Revision: 1, AuthorizingMessage: "message"}, ""); err == nil || !strings.Contains(err.Error(), "brief key must be default") {
		t.Fatalf("migration to the unrunnable revision itself: %v", err)
	}
	r, err := call(t, d, "Migrate", MigrateParams{ID: "pinned", Revision: 2, AuthorizingMessage: "message"}, "")
	if err != nil || !strings.Contains(string(r.Result), `"revision":2`) {
		t.Fatalf("migration out of quarantine: %s %v", r.Result, err)
	}
	d.mu.Lock()
	_, still := d.quarantined["pinned"]
	_, loaded = d.jobs["pinned"]
	d.mu.Unlock()
	if still || !loaded {
		t.Fatalf("after migration: quarantined %t, in the drive %t", still, loaded)
	}
	done := awaitState(t, d, "pinned", "Done")
	if done.WorkflowRevision != 2 {
		t.Fatal("the migrated task did not finish on revision 2", done.WorkflowRevision)
	}
	if _, err = call(t, d, "QuarantineRetire", QuarantineParams{ID: "pinned-too", Reason: "legacy revision; superseded", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	stop(t, d)

	cfg.QuarantineUnrunnableRevisions = false
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal("with no active task left on it, the revision refused the start:", err)
	}
	check := d.startupReport.Load().(StartupChecks).Revisions
	if len(check.Invalid) != 1 || check.Invalid[0].Revision != 1 || len(check.Invalid[0].Tasks) != 0 || check.Invalid[0].Admits || len(check.Quarantined) != 0 {
		t.Fatalf("listing after the tasks left: %+v", check)
	}
	stop(t, d)
	cfg.QuarantineUnrunnableRevisions = true
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	d.mu.Lock()
	quarantined := len(d.quarantined)
	d.mu.Unlock()
	if check = d.startupReport.Load().(StartupChecks).Revisions; quarantined != 0 || len(check.Quarantined) != 0 {
		t.Fatalf("the flag with nothing to quarantine: %d quarantined, %+v", quarantined, check)
	}
}

// FAE-WF-093: a refused start checks stored workflow revisions before
// Migrate upgrades an older plane schema or any lifecycle event or marker is
// written, so the older binary the refusal names ("Or start the binary that
// wrote the revision again") can still open its schema-6 plane.
func TestRefusedStartPreservesOlderPlaneSchemaAndWritesNoLifecycleEvents(t *testing.T) {
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
	plantPinnedTask(t, d, cfg, "pinned", "legacy", 1, legacy)
	if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "simulate schema 6 plane", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM migrations WHERE version > 6`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM events WHERE kind IN ('daemon-starting','dolt-started')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	revisions := countRows(t, d, `SELECT COUNT(*) FROM workflows`)
	jobVersions := countRows(t, d, `SELECT SUM(version) FROM jobs`)
	stop(t, d)

	if d, err = Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("expected start refusal on unrunnable revision")
	}
	layout, err := Paths(cfg.Root, cfg.Instance)
	if err != nil {
		t.Fatal(err)
	}
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
	if got := sqlOut(`SELECT COALESCE(MAX(version),0) FROM migrations`); got != "6" {
		t.Fatalf("refused start migrated schema from 6 to %s", got)
	}
	if got := sqlOut(`SELECT COUNT(*) FROM events WHERE kind IN ('daemon-starting','dolt-started')`); got != "0" {
		t.Fatalf("refused start wrote %s lifecycle events, want 0", got)
	}
	if got := sqlOut(`SELECT COUNT(*) FROM workflows`); got != fmt.Sprint(revisions) {
		t.Fatalf("workflows count = %s, want %d", got, revisions)
	}
	if got := sqlOut(`SELECT SUM(version) FROM jobs`); got != fmt.Sprint(jobVersions) {
		t.Fatalf("jobs version sum = %s, want %d", got, jobVersions)
	}
	if got := sqlOut(`SELECT COUNT(*) FROM events WHERE kind='unrunnable-workflow-revision'`); got != "0" {
		t.Fatalf("unrunnable-workflow-revision events on schema 6 = %s, want 0 (older-schema DB must remain unmutated)", got)
	}
	alertsBytes, err := os.ReadFile(filepath.Join(layout.Root, "alerts.log"))
	if err != nil {
		t.Fatalf("read alerts.log: %v", err)
	}
	if !strings.Contains(string(alertsBytes), "unrunnable-workflow-revision") {
		t.Fatalf("alerts.log missing unrunnable-workflow-revision: %s", alertsBytes)
	}
}

// FAE-WF-093: once `serve --quarantine-unrunnable-revisions` has quarantined
// the active tasks pinned to an unrunnable revision, a subsequent normal
// restart (without the flag) keeps those tasks quarantined instead of
// refusing again before the operator finishes migrating or retiring them, and
// RelaunchArgs strips the one-shot flag from controller maintenance LaunchSpec.
func TestQuarantineUnrunnableRevisionsPersistsAcrossRestartAndStripsFromRelaunch(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	origArgs := append([]string(nil), os.Args...)
	os.Args = []string{origArgs[0], "serve", "--workspace", cfg.Workspace, "--quarantine-unrunnable-revisions", "--adapter", "scripted"}
	t.Cleanup(func() { os.Args = origArgs })
	if _, err = call(t, d, "Controller", map[string]any{"action": "preflight-failed", "note": "gate failed", "marker": map[string]any{"launcher": filepath.Join(cfg.Workspace, "fae")}, "authorizing_message": "message"}, ""); err != nil {
		t.Fatal(err)
	}
	health := d.controllerHealth()
	if health.Maintenance == nil || slices.Contains(health.Maintenance.Launch.Arguments, "--quarantine-unrunnable-revisions") {
		t.Fatalf("maintenance Launch.Arguments kept one-shot flag: %+v", health.Maintenance)
	}

	legacy := unrunnableLegacyDocument("legacy")
	raw, _ := json.Marshal(legacy)
	if _, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "older binary install", Outcome: "ok"}, "legacy", "legacy", raw, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	valid := defaults.Shipped()
	valid.ID = "legacy"
	validRaw, _ := json.Marshal(valid)
	if _, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "valid revision", Outcome: "ok"}, "legacy", "legacy", validRaw, validWorkflow); err != nil {
		t.Fatal(err)
	}
	plantPinnedTask(t, d, cfg, "pinned", "legacy", 1, legacy)
	stop(t, d)

	cfg.QuarantineUnrunnableRevisions = true
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal("start with quarantine flag failed:", err)
	}
	stop(t, d)

	// Restart WITHOUT --quarantine-unrunnable-revisions while "pinned" is still quarantined.
	cfg.QuarantineUnrunnableRevisions = false
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal("subsequent start without flag refused already-quarantined task:", err)
	}
	d.mu.Lock()
	reason := d.quarantined["pinned"]
	_, loaded := d.jobs["pinned"]
	d.mu.Unlock()
	if loaded || !strings.HasPrefix(reason, "unrunnable-revision: workflow legacy revision 1") {
		t.Fatalf("after restart without flag: loaded=%t quarantined=%q", loaded, reason)
	}
	if check := d.startupReport.Load().(StartupChecks).Revisions; fmt.Sprint(check.Quarantined) != "[pinned]" {
		t.Fatalf("startup_checks.revisions.quarantined = %v, want [pinned]", check.Quarantined)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM audit WHERE actor='daemon' AND action='task-quarantined' AND outcome='alert'`); n != 1 {
		t.Fatalf("task-quarantined alerts = %d, want 1 (no duplicate on restart)", n)
	}
	if _, err = call(t, d, "QuarantineRelease", QuarantineParams{ID: "pinned", AuthorizingMessage: "message"}, ""); err == nil || !strings.Contains(err.Error(), "cannot be released") {
		t.Fatalf("release after restart should still refuse: %v", err)
	}
	// A newly planted un-quarantined task on the unrunnable revision still refuses without the flag.
	plantPinnedTask(t, d, cfg, "unquarantined-new", "legacy", 1, legacy)
	stop(t, d)
	if d, err = Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("new unquarantined task on unrunnable revision did not refuse start without flag")
	}
}

// FAE-WF-093: a terminal task (Done) loaded only for pending ticket-outbox
// delivery never executes workflow stages again and must not be counted as an
// active pin that refuses the start or gets quarantined.
func TestTerminalTicketedTaskOnUnrunnableRevisionDoesNotRefuseOrQuarantine(t *testing.T) {
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
	plantPinnedTask(t, d, cfg, "done-ticketed", "legacy", 1, legacy)
	if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "mark task Done with pending ticket", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		var digest string
		var size int64
		if err := tx.QueryRowContext(ctx, `SELECT payload_digest,payload_size FROM jobs WHERE job_id='done-ticketed'`).Scan(&digest, &size); err != nil {
			return err
		}
		payload, err := d.store.GetBlob(ctx, controlplane.BlobRef{Digest: digest, Size: size})
		if err != nil {
			return err
		}
		var e envelope
		if err := json.Unmarshal(payload, &e); err != nil {
			return err
		}
		e.Job.State = "Done"
		updated, _ := json.Marshal(e)
		newDigest := controlplane.Digest(updated)
		if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO blobs (digest,size,chunks) VALUES (?,?,1)`, newDigest, len(updated)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO blob_chunks (digest,chunk_index,data) VALUES (?,0,?)`, newDigest, updated); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state='Done',payload_digest=?,payload_size=? WHERE job_id='done-ticketed'`, newDigest, len(updated)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state='Done',disposition='Done' WHERE task_id='done-ticketed'`); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO ticket_outbox (outbox_key,task_id,room,position,ticket_id,desired_step,started,emitted,attempts,updated_at) VALUES ('done-ticketed:Done','done-ticketed','room',0,'TICK-1','Done',FALSE,FALSE,0,UTC_TIMESTAMP(6))`)
		return err
	}); err != nil {
		stop(t, d)
		t.Fatal(err)
	}
	stop(t, d)

	cfg.QuarantineUnrunnableRevisions = false
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal("Done task with pending ticket refused the start:", err)
	}
	check := d.startupReport.Load().(StartupChecks).Revisions
	if len(check.Invalid) != 1 || len(check.Invalid[0].Tasks) != 0 || len(check.Quarantined) != 0 {
		t.Fatalf("Done ticketed task counted as active pin: %+v", check)
	}
	stop(t, d)

	cfg.QuarantineUnrunnableRevisions = true
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	d.mu.Lock()
	quarantined := len(d.quarantined)
	d.mu.Unlock()
	if check = d.startupReport.Load().(StartupChecks).Revisions; quarantined != 0 || len(check.Quarantined) != 0 {
		t.Fatalf("Done ticketed task quarantined under flag: %d, %+v", quarantined, check)
	}
}

// FAE-WF-093, FAE-ROB-042: a parent task whose SubWorkflow stage pins an
// unrunnable child revision is counted as an active pin at startup;
// adoptMigrated clears unrunnableRevisionTasks once the store migration
// commits even if loadJob re-quarantines the task for another reason; and
// validateStoredJob validates a non-terminal job's embedded workflow document.
func TestSubWorkflowPinEmbeddedDocumentValidationAndAdoptMigratedCleanup(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	childBroken := unrunnableLegacyDocument("child")
	childBrokenRaw, _ := json.Marshal(childBroken)
	if _, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "plant unrunnable child", Outcome: "ok"}, "child", "child", childBrokenRaw, func([]byte) error { return nil }); err != nil {
		stop(t, d)
		t.Fatal(err)
	}
	childValid := childWorkflow()
	childValidRaw, _ := json.Marshal(childValid)
	if _, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "plant valid child", Outcome: "ok"}, "child", "child", childValidRaw, validWorkflow); err != nil {
		stop(t, d)
		t.Fatal(err)
	}
	parent1 := parentWorkflow(1)
	parent1Raw, _ := json.Marshal(parent1)
	if _, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "plant parent rev 1", Outcome: "ok"}, "parent", "parent", parent1Raw, validWorkflow); err != nil {
		stop(t, d)
		t.Fatal(err)
	}
	parent2 := parentWorkflow(2)
	parent2Raw, _ := json.Marshal(parent2)
	if _, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "plant parent rev 2", Outcome: "ok"}, "parent", "parent", parent2Raw, validWorkflow); err != nil {
		stop(t, d)
		t.Fatal(err)
	}
	plantPinnedTask(t, d, cfg, "parent-task", "parent", 1, parent1)
	stop(t, d)

	if d, err = Start(ctx, cfg); err == nil || !strings.Contains(err.Error(), "workflow child revision 1") || !strings.Contains(err.Error(), "active tasks [parent-task] are pinned to it") {
		if d != nil {
			stop(t, d)
		}
		t.Fatalf("indirect sub-workflow pin did not refuse start: %v", err)
	}

	cfg.QuarantineUnrunnableRevisions = true
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// Damage parent-task's ExecutionKind so Migrate commits revision 2 in the
	// store, then adoptMigrated's loadJob fails with executor-kind mismatch.
	if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "set live execution kind on parent-task", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		var digest string
		var size int64
		if err := tx.QueryRowContext(ctx, `SELECT payload_digest,payload_size FROM jobs WHERE job_id='parent-task'`).Scan(&digest, &size); err != nil {
			return err
		}
		payload, err := d.store.GetBlob(ctx, controlplane.BlobRef{Digest: digest, Size: size})
		if err != nil {
			return err
		}
		var e envelope
		if err := json.Unmarshal(payload, &e); err != nil {
			return err
		}
		e.Job.ExecutionKind = "live"
		updated, _ := json.Marshal(e)
		newDigest := controlplane.Digest(updated)
		if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO blobs (digest,size,chunks) VALUES (?,?,1)`, newDigest, len(updated)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO blob_chunks (digest,chunk_index,data) VALUES (?,0,?)`, newDigest, updated); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET payload_digest=?,payload_size=? WHERE job_id='parent-task'`, newDigest, len(updated))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Migrate", MigrateParams{ID: "parent-task", Revision: 2, AuthorizingMessage: "message"}, ""); err == nil || !strings.Contains(err.Error(), "migrated to revision 2 but stays quarantined") {
		t.Fatalf("expected adoptMigrated re-quarantine error, got %v", err)
	}
	d.mu.Lock()
	_, stillUnrunnable := d.unrunnableRevisionTasks["parent-task"]
	newReason := d.quarantined["parent-task"]
	d.mu.Unlock()
	if stillUnrunnable || !strings.Contains(newReason, "stored job executor kind live differs from daemon mode scripted") {
		t.Fatalf("adoptMigrated left unrunnableRevisionTasks=%t or old reason=%q", stillUnrunnable, newReason)
	}
	if _, err = call(t, d, "QuarantineRelease", QuarantineParams{ID: "parent-task", AuthorizingMessage: "message"}, ""); err == nil || strings.Contains(err.Error(), "cannot be run by this binary") || !strings.Contains(err.Error(), "stored job executor kind live differs from daemon mode scripted") {
		t.Fatalf("QuarantineRelease after migration reported stale unrunnable-revision error: %v", err)
	}
	if _, err = call(t, d, "QuarantineRetire", QuarantineParams{ID: "parent-task", Reason: "retire mismatched task", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}

	// Plant a task on valid revision 2 of child whose embedded Job.Document is damaged.
	damagedDoc := defaults.Shipped()
	damagedDoc.ID = "child"
	damagedStage := damagedDoc.Stages[damagedDoc.Start]
	damagedStage.Primitive = workflow.Gate
	damagedStage.Expr = nil
	damagedDoc.Stages[damagedDoc.Start] = damagedStage
	plantPinnedTask(t, d, cfg, "embedded-damaged", "child", 2, damagedDoc)
	stop(t, d)

	cfg.QuarantineUnrunnableRevisions = false
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	d.mu.Lock()
	embeddedReason := d.quarantined["embedded-damaged"]
	_, embeddedLoaded := d.jobs["embedded-damaged"]
	d.mu.Unlock()
	if embeddedLoaded || !strings.Contains(embeddedReason, "expression: required") {
		t.Fatalf("embedded damaged document: loaded=%t quarantined=%q", embeddedLoaded, embeddedReason)
	}
}

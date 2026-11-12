package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

func retentionFixture(t *testing.T) *Daemon {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "backup"), 0700); err != nil {
		t.Fatal(err)
	}
	return &Daemon{config: Config{Instance: "retention", Workspace: "/fixture"}, layout: Layout{Root: root}}
}
func retainedFixtureBackup(t *testing.T, d *Daemon, name string, created int64, jobs ...backupJob) planeBackup {
	t.Helper()
	path := filepath.Join(d.layout.Root, "backup", "plane-"+name)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	content := []byte("restored verified test payload")
	if err := os.WriteFile(filepath.Join(path, "data"), content, 0600); err != nil {
		t.Fatal(err)
	}
	b := planeBackup{Path: path, Instance: d.config.Instance, Workspace: d.config.Workspace, Created: time.Unix(created, 0).UTC(), Jobs: jobs, Files: map[string]string{"data": controlplane.Digest(content)}}
	raw, _ := json.Marshal(b)
	if err := os.WriteFile(filepath.Join(path, "verified.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return b
}
func TestBackupRetentionCoverageAndApply(t *testing.T) {
	d := retentionFixture(t)
	ctx := context.Background()
	job := backupJob{ID: "gone", Version: 1, State: "Retired", Digest: controlplane.Digest([]byte("retired"))}
	older := retainedFixtureBackup(t, d, "older", 1, job)
	covered := retainedFixtureBackup(t, d, "covered", 2, job)
	newest := retainedFixtureBackup(t, d, "newest", 3)
	preview, e := d.backupRetention(ctx, BackupRetentionParams{Keep: 1})
	if e != nil || len(preview.Remove) != 1 || preview.Remove[0] != older.Path || len(preview.Retained) != 2 {
		t.Fatal(preview, e)
	}
	if _, e = os.Stat(older.Path); e != nil {
		t.Fatal("preview mutated", e)
	}
	if _, e = d.backupRetention(ctx, BackupRetentionParams{Keep: 1, Apply: true, PlanDigest: "bad"}); e == nil {
		t.Fatal("missing exact plan accepted")
	}
	result, e := d.backupRetention(ctx, BackupRetentionParams{Keep: 1, Apply: true, PlanDigest: preview.PlanDigest})
	if e != nil || len(result.Removed) != 1 {
		t.Fatal(result, e)
	}
	if _, e = os.Stat(older.Path); !os.IsNotExist(e) {
		t.Fatal(e)
	}
	for _, path := range []string{covered.Path, newest.Path} {
		if _, e = d.readBackup(ctx, path); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = d.backupRetention(ctx, BackupRetentionParams{Keep: 1, Apply: true, PlanDigest: preview.PlanDigest}); e == nil {
		t.Fatal("stale plan replay")
	}
}
func TestBackupRetentionFailClosed(t *testing.T) {
	for _, mode := range []string{"candidate-corrupt", "survivor-corrupt", "symlink", "unknown", "missing-manifest", "replaced-directory", "rewritten-manifest", "new-backup", "permissions"} {
		t.Run(mode, func(t *testing.T) {
			d := retentionFixture(t)
			ctx := context.Background()
			old := retainedFixtureBackup(t, d, "old", 1)
			newest := retainedFixtureBackup(t, d, "new", 2)
			plan, e := d.backupRetention(ctx, BackupRetentionParams{Keep: 1})
			if e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "candidate-corrupt":
				e = os.WriteFile(filepath.Join(old.Path, "data"), []byte("changed"), 0600)
			case "survivor-corrupt":
				e = os.WriteFile(filepath.Join(newest.Path, "data"), []byte("changed"), 0600)
			case "symlink":
				e = os.Symlink(newest.Path, filepath.Join(d.layout.Root, "backup", "plane-link"))
			case "unknown":
				e = os.WriteFile(filepath.Join(d.layout.Root, "backup", "unknown"), nil, 0600)
			case "missing-manifest":
				e = os.Remove(filepath.Join(old.Path, "verified.json"))
			case "replaced-directory":
				e = os.Rename(old.Path, filepath.Join(d.layout.Root, "saved"))
				if e == nil {
					retainedFixtureBackup(t, d, "old", 1)
				}
			case "rewritten-manifest":
				raw, _ := os.ReadFile(filepath.Join(old.Path, "verified.json"))
				e = os.WriteFile(filepath.Join(old.Path, "verified.json"), append(raw, '\n'), 0600)
			case "new-backup":
				retainedFixtureBackup(t, d, "third", 3)
			case "permissions":
				e = os.Chmod(old.Path, 0777)
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = d.backupRetention(ctx, BackupRetentionParams{Keep: 1, Apply: true, PlanDigest: plan.PlanDigest}); e == nil {
				t.Fatal("unsafe apply accepted")
			}
			if _, e = os.Stat(old.Path); e != nil {
				t.Fatal("candidate deleted", e)
			}
		})
	}
}
func TestBackupRetentionGuardAndBounds(t *testing.T) {
	for _, verified := range []bool{false, true} {
		for _, protected := range []bool{false, true} {
			for _, same := range []bool{false, true} {
				if CanPruneBackup(verified, protected, same) != (verified && !protected && same) {
					t.Fatal(verified, protected, same)
				}
			}
		}
	}
	d := retentionFixture(t)
	for _, p := range []BackupRetentionParams{{Keep: 0}, {Keep: -1}, {Keep: 10001}, {Keep: 1, Apply: true, DryRun: true}} {
		if _, e := d.backupRetention(context.Background(), p); e == nil {
			t.Fatal(p)
		}
	}
	// Every historical state is preserved even when no current job references it.
	j := backupJob{ID: "old", Version: 2, State: "Ready", Digest: controlplane.Digest(nil)}
	retainedFixtureBackup(t, d, "a", 1, j)
	retainedFixtureBackup(t, d, "b", 2)
	plan, e := d.backupRetention(context.Background(), BackupRetentionParams{Keep: 1})
	if e != nil || len(plan.Remove) != 0 {
		t.Fatal(plan, e)
	}
}
func TestBackupRetentionNativeVerifiedBackups(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, e = call(t, d, "BackupRetention", BackupRetentionParams{Keep: 1, Apply: true, PlanDigest: controlplane.Digest(nil)}, ""); e == nil {
		t.Fatal("unauthorized retention")
	}
	first, e := backupAdmitted(t, ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	second, e := backupAdmitted(t, ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	plan, e := d.backupRetention(ctx, BackupRetentionParams{Keep: 1})
	if e != nil || len(plan.Remove) != 1 || plan.Remove[0] != first.Path {
		t.Fatal(plan, e)
	}
	response, e := call(t, d, "BackupRetention", BackupRetentionParams{Keep: 1, Apply: true, PlanDigest: plan.PlanDigest, AuthorizingMessage: "message"}, "")
	if e != nil {
		t.Fatal(e)
	}
	var result BackupRetentionResult
	if e = json.Unmarshal(response.Result, &result); e != nil || len(result.Removed) != 1 {
		t.Fatal(result, e)
	}
	if _, e = call(t, d, "BackupRetention", BackupRetentionParams{Keep: 1, Apply: true, PlanDigest: plan.PlanDigest, AuthorizingMessage: "message"}, ""); e == nil {
		t.Fatal("stale native preview accepted")
	}
	if _, e = d.readBackup(ctx, second.Path); e != nil {
		t.Fatal(e)
	}
}

func TestBackupRetentionSerialization(t *testing.T) {
	d := retentionFixture(t)
	d.backupMu.Lock()
	defer d.backupMu.Unlock()
	if _, e := d.backupRetention(context.Background(), BackupRetentionParams{Keep: 1}); e == nil {
		t.Fatal("concurrent retention")
	}
	if _, e := backupAdmitted(t, context.Background(), d); e == nil {
		t.Fatal("concurrent backup")
	}
	if _, e := d.cleanupRetired(context.Background(), CleanupParams{}); e == nil {
		t.Fatal("concurrent cleanup")
	}
}

func TestBackupRetentionNativeHistoricalCoverage(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	job, e := engine.NewLive("historical", 1, defaults.Workflow(), liveGit(t, cfg.Workspace, "rev-parse", "HEAD^{tree}"))
	if e != nil {
		t.Fatal(e)
	}
	job.State = "Retired"
	raw, _ := json.Marshal(envelope{Job: job, Admission: AdmitParams{ID: "historical", RunID: "retention"}})
	audit := controlplane.Audit{Actor: "operator", Action: "retention fixture", Outcome: "ok"}
	_, e = d.store.AdmitJob(ctx, audit, "historical", controlplane.JobSnapshot{ID: "historical", WorkflowID: job.Document.ID, WorkflowRevision: 1, TargetID: cfg.Workspace, TargetRevision: 1, State: "Retired", Payload: raw})
	if e != nil {
		t.Fatal(e)
	}
	oldest, e := backupAdmitted(t, ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	e = d.store.Mutate(ctx, audit, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE job_id=?`, "historical")
		return err
	})
	if e != nil {
		t.Fatal(e)
	}
	duplicate, e := backupAdmitted(t, ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	newest, e := backupAdmitted(t, ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	plan, e := d.backupRetention(ctx, BackupRetentionParams{Keep: 1})
	if e != nil || len(plan.Remove) != 1 || plan.Remove[0] != duplicate.Path {
		t.Fatal(plan, e)
	}
	result, e := d.backupRetention(ctx, BackupRetentionParams{Keep: 1, Apply: true, PlanDigest: plan.PlanDigest})
	if e != nil || len(result.Removed) != 1 {
		t.Fatal(result, e)
	}
	for _, path := range []string{oldest.Path, newest.Path} {
		if _, e = d.readBackup(ctx, path); e != nil {
			t.Fatal(e)
		}
	}
}

func TestBackupRetentionNativeReplicationPin(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	older := retainedFixtureBackup(t, d, "pending", 1)
	retainedFixtureBackup(t, d, "latest", 2)
	before, e := d.backupRetention(ctx, BackupRetentionParams{Keep: 1})
	if e != nil || len(before.Remove) != 1 {
		t.Fatal(before, e)
	}
	raw, _ := json.Marshal([]string{older.Path})
	if e = d.store.PutMarker(ctx, controlplane.Audit{Actor: "operator", Action: "pending replication fixture", Outcome: "ok"}, "backup-replication-sources", raw); e != nil {
		t.Fatal(e)
	}
	if _, e = d.backupRetention(ctx, BackupRetentionParams{Keep: 1, Apply: true, PlanDigest: before.PlanDigest}); e == nil {
		t.Fatal("journal changed after preview")
	}
	plan, e := d.backupRetention(ctx, BackupRetentionParams{Keep: 1})
	if e != nil || len(plan.Remove) != 0 || len(plan.Retained) != 2 {
		t.Fatal(plan, e)
	}
	if _, e = d.backupRetention(ctx, BackupRetentionParams{Keep: 1, Apply: true, PlanDigest: plan.PlanDigest}); e != nil {
		t.Fatal(e)
	}
	if _, e = d.readBackup(ctx, older.Path); e != nil {
		t.Fatal(e)
	}
}

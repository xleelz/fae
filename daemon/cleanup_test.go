package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/worktree"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupNativeBackupRetiredAndRestart(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			d.Close(context.Background())
		}
	}()
	m := &worktree.Manager{Repository: cfg.Workspace, Root: d.layout.Worktrees}
	binding, err := m.Ensure(ctx, "cleanup-run", "cleanup-task", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	liveGit(t, binding.Path, "commit", "--allow-empty", "-m", "task only")
	job, err := engine.NewLive("cleanup-task", 1, defaults.Workflow(), liveGit(t, binding.Path, "rev-parse", "HEAD^{tree}"))
	if err != nil {
		t.Fatal(err)
	}
	job.State = "Retired"
	e := envelope{Job: job, Admission: AdmitParams{ID: "cleanup-task", RunID: "cleanup-run"}}
	raw, _ := json.Marshal(e)
	j, err := d.store.AdmitJob(ctx, controlplane.Audit{Actor: "operator", Action: "test retired", Outcome: "ok"}, "cleanup", controlplane.JobSnapshot{ID: e.Job.TaskID, WorkflowID: defaults.Workflow().ID, WorkflowRevision: 1, TargetID: cfg.Workspace, TargetRevision: 1, State: "Retired", Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	dry, err := d.cleanupRetired(ctx, CleanupParams{})
	if err != nil || len(dry) != 1 || dry[0].Removed {
		t.Fatal(dry, err)
	}
	if _, err = d.cleanupRetired(ctx, CleanupParams{Apply: true}); err == nil {
		t.Fatal("missing backup accepted")
	}
	authorize(t, d)
	for _, grant := range []struct{ id, text string }{{"restricted", `{"text":"task only","authorize":{"verbs":["Admit"]}}`}, {"used", "task action already consumed"}, {"runtime", `{"text":"backup","authorize":{"classes":["runtime"]}}`}} {
		if err = d.store.RecordMessage(ctx, grant.id, grant.text); err != nil {
			t.Fatal(err)
		}
	}
	authority, err := d.store.AuthorizeAction(ctx, "used", "operator", "Admit", "task", false, actionClasses())
	if err != nil {
		t.Fatal(err)
	}
	if err = d.store.PutMarker(authority, controlplane.Audit{Actor: "operator", Action: "test consumption", MessageID: "used", Outcome: "ok"}, "cleanup-auth-test", json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"PlaneBackup", "CleanupRetired"} {
		for _, id := range []string{"restricted", "used", "missing"} {
			if _, err = call(t, d, method, CleanupParams{Apply: true, Backup: "/not-reached", AuthorizingMessage: id}, ""); err == nil {
				t.Fatal("unauthorized cleanup accepted", method, id)
			}
		}
	}
	if err = d.store.AuthorizeReadOnlyEffect(ctx, "runtime", "operator", "PlaneBackup", "runtime", actionClasses()); err != nil {
		t.Fatal(err)
	}
	response, err := call(t, d, "PlaneBackup", CleanupParams{AuthorizingMessage: "message"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var b planeBackup
	if err = json.Unmarshal(response.Result, &b); err != nil {
		t.Fatal(err)
	}
	j, err = d.store.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !backedUp(b, j) {
		t.Fatalf("backup omitted retired job: %+v %+v", b.Jobs, j)
	}
	// A missing verified manifest must refuse before any Git deletion.
	manifest := filepath.Join(b.Path, "verified.json")
	hidden := filepath.Join(b.Path, "saved-manifest")
	if err = os.Rename(manifest, hidden); err != nil {
		t.Fatal(err)
	}
	if _, err = d.cleanupRetired(ctx, CleanupParams{Apply: true, Backup: b.Path}); err == nil {
		t.Fatal("backup failure accepted")
	}
	if _, err = os.Stat(binding.Path); err != nil {
		t.Fatal("worktree lost on backup failure", err)
	}
	if err = os.Rename(hidden, manifest); err != nil {
		t.Fatal(err)
	}
	before, err := d.store.ListJobs(ctx, 100000)
	if err != nil {
		t.Fatal(err)
	}
	items, err := d.cleanupRetired(ctx, CleanupParams{Apply: true, Backup: b.Path})
	if err != nil || len(items) != 1 || !items[0].Removed {
		t.Fatal(items, err)
	}
	after, err := d.store.ListJobs(ctx, 100000)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(before)
	z, _ := json.Marshal(after)
	if string(a) != string(z) {
		t.Fatal("cleanup changed plane jobs")
	}
	if err = d.Close(ctx); err != nil {
		t.Fatal(err)
	}
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	items, err = d.cleanupRetired(ctx, CleanupParams{Apply: true, Backup: b.Path})
	if err != nil || len(items) != 1 || !items[0].Plan.AlreadyRemoved {
		t.Fatal(items, err)
	}
	if err = os.WriteFile(filepath.Join(b.Path, "remote", "foreign"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = d.cleanupRetired(ctx, CleanupParams{Apply: true, Backup: b.Path}); err == nil {
		t.Fatal("changed backup accepted")
	}
}
func TestCleanupBackupOwnershipAndPacing(t *testing.T) {
	d := &Daemon{layout: Layout{Root: t.TempDir()}}
	if _, err := d.readBackup(context.Background(), "/unowned"); err == nil {
		t.Fatal("foreign backup")
	}
	for _, pace := range []int{-1, 1, 60001} {
		if _, err := d.cleanupRetired(context.Background(), CleanupParams{PaceMS: pace}); err == nil {
			t.Fatal("bad pacing")
		}
	}
	if _, err := d.cleanupRetired(context.Background(), CleanupParams{Apply: true, DryRun: true}); err == nil {
		t.Fatal("conflicting flags")
	}
}

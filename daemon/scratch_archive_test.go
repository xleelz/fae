package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/worktree"
)

func TestScratchArchiveGuard(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			for _, c := range []bool{false, true} {
				if CanRemoveScratchArchive(a, b, c) != (a && b && c) {
					t.Fatal(a, b, c)
				}
			}
		}
	}
}
func scratchArchiveFixture(t *testing.T) (ScratchArchiveCleanupParams, ScratchCleanupResult) {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	parent := filepath.Join(home, ".scratch-archive")
	os.Mkdir(parent, 0700)
	identity := ScratchIdentity{ID: strings.Repeat("a", 32), Instance: "fixture", Root: filepath.Join(home, "fixture"), Workspace: home, Created: time.Unix(100, 0)}
	canonical := filepath.Join(parent, identity.ID+"-canonical")
	os.Mkdir(canonical, 0700)
	os.WriteFile(filepath.Join(canonical, "payload"), []byte("recoverable bytes"), 0600)
	files, e := scratchFiles(context.Background(), canonical)
	if e != nil {
		t.Fatal(e)
	}
	journal := ScratchCleanupResult{Identity: identity, ArchivePath: canonical, Prepared: true, Completed: true, Files: files}
	if e = scratchSave(home, journal); e != nil {
		t.Fatal(e)
	}
	source := filepath.Join(parent, identity.ID+"-duplicate")
	if e = scratchCopy(context.Background(), canonical, source); e != nil {
		t.Fatal(e)
	}
	return ScratchArchiveCleanupParams{Home: home, Instance: identity.Instance, Source: source, Survivor: canonical}, journal
}
func TestScratchArchiveRefusalsAndStalePlan(t *testing.T) {
	for _, mode := range []string{"canonical", "foreign-survivor", "extra-file", "changed-survivor", "pending", "active", "staged", "symlink", "public", "missing-digest", "stale-inode"} {
		t.Run(mode, func(t *testing.T) {
			p, j := scratchArchiveFixture(t)
			ctx := context.Background()
			preview, e := CleanupScratchArchive(ctx, p)
			if e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "canonical":
				p.Source, p.Survivor = p.Survivor, p.Source
			case "foreign-survivor":
				p.Survivor = p.Source
			case "extra-file":
				os.WriteFile(filepath.Join(p.Source, "unique"), []byte("do not lose"), 0600)
			case "changed-survivor":
				os.WriteFile(filepath.Join(p.Survivor, "payload"), []byte("corrupt"), 0600)
			case "pending":
				j.Completed = false
				scratchSave(p.Home, j)
			case "active":
				os.Mkdir(j.Identity.Root, 0700)
			case "staged":
				os.MkdirAll(filepath.Join(p.Home, ".scratch-removing", j.Identity.ID), 0700)
			case "symlink":
				os.Rename(p.Source, p.Source+"-saved")
				os.Symlink(p.Source+"-saved", p.Source)
			case "public":
				os.Chmod(p.Source, 0755)
			case "stale-inode":
				os.Rename(p.Source, p.Source+"-saved")
				scratchCopy(ctx, p.Survivor, p.Source)
			}
			p.Apply = true
			p.PlanDigest = preview.PlanDigest
			if mode == "missing-digest" {
				p.PlanDigest = ""
			}
			if _, e = CleanupScratchArchive(ctx, p); e == nil {
				t.Fatal("unsafe archive deletion accepted")
			}
			if _, e = os.Lstat(p.Source); e != nil {
				t.Fatal("refused source deleted", e)
			}
		})
	}
}
func scratchArchiveSimulatePartialReceipt(t *testing.T, p ScratchArchiveCleanupParams, digest string) {
	t.Helper()
	ctx := context.Background()
	journal, jh, e := scratchArchiveJournal(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	src, e := scratchArchiveDirectory(p.Source)
	if e != nil {
		t.Fatal(e)
	}
	dst, e := scratchArchiveDirectory(p.Survivor)
	if e != nil {
		t.Fatal(e)
	}
	plan := scratchArchivePlan{p.Home, p.Instance, p.Source, p.Survivor, jh, src, dst, journal.Files}
	if scratchArchivePlanDigest(plan) != digest {
		t.Fatal("fixture plan differs")
	}
	dir := filepath.Join(p.Home, ".scratch-archive", ".removals")
	if e = mkdirPrivate(dir); e != nil {
		t.Fatal(e)
	}
	if e = scratchArchiveSave(filepath.Join(dir, digest+".json"), scratchArchiveReceipt{Plan: plan, PlanDigest: digest}); e != nil {
		t.Fatal(e)
	}
	for name := range journal.Files {
		if e = os.Remove(filepath.Join(p.Source, name)); e != nil {
			t.Fatal(e)
		}
		break
	}
}
func TestScratchArchivePartialRecoveryAndReceiptTamper(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "tamper"}[tamper], func(t *testing.T) {
			p, _ := scratchArchiveFixture(t)
			preview, e := CleanupScratchArchive(context.Background(), p)
			if e != nil {
				t.Fatal(e)
			}
			scratchArchiveSimulatePartialReceipt(t, p, preview.PlanDigest)
			p.Apply = true
			p.PlanDigest = preview.PlanDigest
			if tamper {
				os.WriteFile(filepath.Join(p.Source, "unique"), []byte("new data"), 0600)
				if _, e = CleanupScratchArchive(context.Background(), p); e == nil {
					t.Fatal("partial drift accepted")
				}
				return
			}
			result, e := CleanupScratchArchive(context.Background(), p)
			if e != nil || !result.Completed {
				t.Fatal(result, e)
			}
			result, e = CleanupScratchArchive(context.Background(), p)
			if e != nil || !result.Completed {
				t.Fatal("repeat", result, e)
			}
			if _, e = os.Stat(p.Survivor); e != nil {
				t.Fatal("canonical lost", e)
			}
			os.Mkdir(p.Source, 0700)
			if _, e = CleanupScratchArchive(context.Background(), p); e == nil {
				t.Fatal("recreated source accepted")
			}
		})
	}
}
func TestScratchArchiveNativeBackupPreservationAndRestart(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.Scratch = true
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	authorize(t, d)
	manager := &worktree.Manager{Repository: cfg.Workspace, Root: d.layout.Worktrees}
	binding, e := manager.Ensure(ctx, "archive-run", "archive-task", "HEAD")
	if e != nil {
		stop(t, d)
		t.Fatal(e)
	}
	liveGit(t, binding.Path, "commit", "--allow-empty", "-m", "scratch archive")
	job, e := engine.NewLive("archive-task", 1, defaults.Workflow(), liveGit(t, binding.Path, "rev-parse", "HEAD^{tree}"))
	if e != nil {
		stop(t, d)
		t.Fatal(e)
	}
	job.State = "Retired"
	raw, _ := json.Marshal(envelope{Job: job, Admission: AdmitParams{ID: job.TaskID, RunID: "archive-run"}})
	_, e = d.store.AdmitJob(ctx, controlplane.Audit{Actor: "operator", Action: "fixture", Outcome: "ok"}, "archive", controlplane.JobSnapshot{ID: job.TaskID, WorkflowID: job.Document.ID, WorkflowRevision: 1, TargetID: cfg.Workspace, TargetRevision: 1, State: "Retired", Payload: raw})
	if e != nil {
		stop(t, d)
		t.Fatal(e)
	}
	stop(t, d)
	cleanup := ScratchCleanupParams{Home: cfg.Root, Instance: cfg.Instance, DoltBinary: cfg.DoltBinary, AuthorizingMessage: "message", Apply: true}
	journal, e := CleanupScratch(ctx, cleanup)
	if e != nil {
		t.Fatal(e)
	}
	source := journal.ArchivePath + "-redundant"
	if e = scratchCopy(ctx, journal.ArchivePath, source); e != nil {
		t.Fatal(e)
	}
	p := ScratchArchiveCleanupParams{Home: cfg.Root, Instance: cfg.Instance, Source: source, Survivor: journal.ArchivePath}
	preview, e := CleanupScratchArchive(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	// Lost Git archive proof must protect both copies.
	plan := journal.Plans[0]
	liveGit(t, cfg.Workspace, "update-ref", "-d", plan.Archive)
	if _, e = CleanupScratchArchive(ctx, p); e == nil {
		t.Fatal("missing Git proof accepted")
	}
	liveGit(t, cfg.Workspace, "update-ref", plan.Archive, plan.Head)
	// Simulate a controller crash after receipt and one removed backup file.
	scratchArchiveSimulatePartialReceipt(t, p, preview.PlanDigest)
	p.Apply = true
	p.PlanDigest = preview.PlanDigest
	result, e := CleanupScratchArchive(ctx, p)
	if e != nil || !result.Completed {
		t.Fatal(result, e)
	}
	if _, e = os.Lstat(source); !os.IsNotExist(e) {
		t.Fatal("duplicate remains", e)
	}
	if e = scratchVerify(ctx, journal); e != nil {
		t.Fatal("sole surviving full backup invalid", e)
	}
	again, e := CleanupScratch(ctx, cleanup)
	if e != nil || !again.Completed || again.ArchivePath != journal.ArchivePath {
		t.Fatal("completed cleanup recovery changed", again, e)
	}
	result, e = CleanupScratchArchive(ctx, p)
	if e != nil || !result.Completed {
		t.Fatal("archive removal retry", result, e)
	}
}

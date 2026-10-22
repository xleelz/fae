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
	"strings"
	"testing"
	"time"
)

func TestScratchNativeStopBackupCleanupAndRepeat(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.Scratch = true
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	scratchTestFIFO(t, d.layout.Root)
	m := &worktree.Manager{Repository: cfg.Workspace, Root: d.layout.Worktrees}
	b, err := m.Ensure(ctx, "scratch-run", "scratch-task", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	liveGit(t, b.Path, "commit", "--allow-empty", "-m", "scratch-only")
	job, err := engine.NewLive("scratch-task", 1, defaults.Workflow(), liveGit(t, b.Path, "rev-parse", "HEAD^{tree}"))
	if err != nil {
		t.Fatal(err)
	}
	job.State = "Retired"
	raw, _ := json.Marshal(envelope{Job: job, Admission: AdmitParams{ID: job.TaskID, RunID: "scratch-run"}})
	if _, err = d.store.AdmitJob(ctx, controlplane.Audit{Actor: "operator", Action: "scratch fixture", Outcome: "ok"}, "scratch", controlplane.JobSnapshot{ID: job.TaskID, WorkflowID: job.Document.ID, WorkflowRevision: 1, TargetID: cfg.Workspace, TargetRevision: 1, State: "Retired", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	p := ScratchCleanupParams{Home: cfg.Root, Instance: cfg.Instance, DoltBinary: cfg.DoltBinary, AuthorizingMessage: "message"}
	dry, err := CleanupScratch(ctx, p)
	if err != nil || dry.Completed || dry.Identity.ID == "" {
		t.Fatal(dry, err)
	}
	if _, err = os.Stat(d.layout.Root); err != nil {
		t.Fatal(err)
	}
	if err = d.store.RecordMessage(ctx, "wrong-grant", `{"text":"admit only","authorize":{"verbs":["Admit"]}}`); err != nil {
		t.Fatal(err)
	}
	wrong := p
	wrong.Apply = true
	wrong.AuthorizingMessage = "wrong-grant"
	if _, err = CleanupScratch(ctx, wrong); err == nil {
		t.Fatal("unrelated grant accepted")
	}
	if _, err = call(t, d, "Status", map[string]any{}, ""); err != nil {
		t.Fatal("unauthorized cleanup stopped daemon", err)
	}
	running := d
	done := make(chan error, 1)
	go func() { done <- running.Run(ctx) }()
	p.Apply = true
	result, err := CleanupScratch(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	d = nil
	if !result.Completed || len(result.Plans) != 1 {
		t.Fatal(result)
	}
	if _, err = os.Stat(result.Identity.Root); !os.IsNotExist(err) {
		t.Fatal("root retained", err)
	}
	if _, err = os.Stat(result.ArchivePath); err != nil {
		t.Fatal("backup lost", err)
	}
	metadata, err := os.ReadFile(filepath.Join(result.ArchivePath, "runtime-endpoints.json"))
	if err != nil {
		t.Fatal(err)
	}
	var endpoints map[string]string
	if err = json.Unmarshal(metadata, &endpoints); err != nil {
		t.Fatal(err)
	}
	endpoint := "sessions/session-test/start.pipe"
	if endpoints[endpoint] == "" || endpoints[endpoint] != result.SourceFiles[endpoint] || result.Files["runtime-endpoints.json"] == "" {
		t.Fatal("IPC metadata is not preserved and verified")
	}
	if got := liveGit(t, cfg.Workspace, "rev-parse", result.Plans[0].Archive); got != result.Plans[0].Head {
		t.Fatal("archive mismatch")
	}
	again, err := CleanupScratch(ctx, p)
	if err != nil || !again.Completed || again.Identity.ID != result.Identity.ID {
		t.Fatal(again, err)
	}
}
func TestScratchNativeCannotRelabelOrdinaryInstance(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = CleanupScratch(ctx, ScratchCleanupParams{Home: cfg.Root, Instance: cfg.Instance, Apply: true}); err == nil {
		t.Fatal("nonscratch accepted")
	}
	stop(t, d)
	cfg.Scratch = true
	if d, err = Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("relabel accepted")
	}
}
func TestScratchNativeFailedBackupDoesNotDelete(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Scratch = true
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	l := d.layout
	stop(t, d)
	p := ScratchCleanupParams{Home: cfg.Root, Instance: cfg.Instance, Apply: true, DoltBinary: "/missing/dolt", AuthorizingMessage: "message"}
	if _, err = CleanupScratch(ctx, p); err == nil {
		t.Fatal("missing verifier accepted")
	}
	if _, err = os.Stat(l.Data); err != nil {
		t.Fatal("plane deleted", err)
	}
	registry := filepath.Join(l.Run, "effect-processes")
	if err = mkdirPrivate(registry); err != nil {
		t.Fatal(err)
	}
	badRecord := filepath.Join(registry, "process-invalid.json")
	if err = os.WriteFile(badRecord, []byte(`{"owner":"foreign"}`), 0600); err != nil {
		t.Fatal(err)
	}
	wrong := p
	wrong.DoltBinary = cfg.DoltBinary
	wrong.AuthorizingMessage = "missing-message"
	if _, e := CleanupScratch(ctx, wrong); e == nil || !strings.Contains(e.Error(), "authorization not found") {
		t.Fatal("process recovery reached before authorization", e)
	}
	if err = os.Remove(badRecord); err != nil {
		t.Fatal(err)
	}
	p.DoltBinary = cfg.DoltBinary
	r, err := CleanupScratch(ctx, p)
	if err != nil || !r.Completed {
		t.Fatal(r, err)
	}
}
func TestScratchGuardAndOwnership(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			if CanRemoveScratch(a, b) != (a && b) {
				t.Fatal(a, b)
			}
		}
	}
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, "test")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := CleanupScratch(ctx, ScratchCleanupParams{Home: home, Instance: "test", Apply: true}); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestScratchNativeArchiveFailureAndStagedRecovery(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.Scratch = true
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	l := d.layout
	m := &worktree.Manager{Repository: cfg.Workspace, Root: l.Worktrees}
	binding, err := m.Ensure(ctx, "review-prepared", "unadmitted", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	liveGit(t, binding.Path, "commit", "--allow-empty", "-m", "unadmitted patch")
	raw, _ := json.Marshal(map[string]any{"task": "unadmitted", "run_id": "review-prepared", "workspace": binding.Path, "branch": binding.Branch, "state": "ready"})
	if err = d.store.PutMarker(ctx, controlplane.Audit{Actor: "operator", Action: "review fixture", Outcome: "ok"}, "review-workspace/"+controlplane.Digest([]byte("unadmitted")), raw); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	block := filepath.Join(cfg.Workspace, ".git", "refs", "fae", "archive", worktree.Identity("review-prepared", "unadmitted"))
	if err = os.MkdirAll(filepath.Dir(block), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(block, []byte("archive collision"), 0600); err != nil {
		t.Fatal(err)
	}
	p := ScratchCleanupParams{Home: cfg.Root, Instance: cfg.Instance, DoltBinary: cfg.DoltBinary, AuthorizingMessage: "message", Apply: true}
	result, err := CleanupScratch(ctx, p)
	if err == nil || !result.Prepared || len(result.Plans) != 1 {
		t.Fatal("archive failure did not preserve prepared backup", result, err)
	}
	if _, err = os.Stat(binding.Path); err != nil {
		t.Fatal("worktree lost", err)
	}
	if err = os.Remove(block); err != nil {
		t.Fatal(err)
	}
	// A drive restart cannot reopen a pending cleanup identity.
	cfg.Scratch = false
	if restarted, e := Start(ctx, cfg); e == nil {
		stop(t, restarted)
		t.Fatal("pending cleanup restarted")
	}
	extra := filepath.Join(l.Root, "late-file")
	if err = os.WriteFile(extra, []byte("new sole copy"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = CleanupScratch(ctx, p); err == nil {
		t.Fatal("source drift accepted")
	}
	if err = os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	// Reproduce interruption after safe Git cleanup and the atomic staging rename.
	for _, plan := range result.Plans {
		if err = m.Cleanup(ctx, plan, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	stageRoot := filepath.Join(cfg.Root, ".scratch-removing")
	if err = mkdirPrivate(stageRoot); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(stageRoot, result.Identity.ID)
	if err = os.Rename(l.Root, stage); err != nil {
		t.Fatal(err)
	}
	preview := p
	preview.Apply = false
	view, e := CleanupScratch(ctx, preview)
	if e != nil || view.Completed {
		t.Fatal("staged preview claims completed", view, e)
	}
	plan := result.Plans[0]
	liveGit(t, cfg.Workspace, "update-ref", "-d", plan.Archive)
	if _, err = CleanupScratch(ctx, p); err == nil {
		t.Fatal("missing final archive accepted")
	}
	if _, err = os.Stat(stage); err != nil {
		t.Fatal("stage deleted without archive", err)
	}
	liveGit(t, cfg.Workspace, "update-ref", plan.Archive, plan.Head)
	late := filepath.Join(stage, "new-file")
	if err = os.WriteFile(late, []byte("sole copy"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = CleanupScratch(ctx, p); err == nil {
		t.Fatal("new staged data deleted")
	}
	if _, err = os.Stat(late); err != nil {
		t.Fatal("new staged file lost", err)
	}
	if err = os.Remove(late); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(stage, "verified.json")
	if err = os.WriteFile(manifest, []byte("also sole copy"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = CleanupScratch(ctx, p); err == nil {
		t.Fatal("unknown staged verified.json accepted")
	}
	if err = os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(stage, "workspace.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(stage, "workspace.json"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = CleanupScratch(ctx, p); err == nil {
		t.Fatal("changed staged data deleted")
	}
	if err = os.WriteFile(filepath.Join(stage, "workspace.json"), original, 0600); err != nil {
		t.Fatal(err)
	}
	// Even a partial staged deletion remains bound by directory device/inode.
	if err = os.Remove(filepath.Join(stage, "scratch.json")); err != nil {
		t.Fatal(err)
	}
	done, err := CleanupScratch(ctx, p)
	if err != nil || !done.Completed {
		t.Fatalf("completed=%v: %v", done.Completed, err)
	}
	if _, err = os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("stage retained", err)
	}
}
func TestScratchJournalAndBackupTamperRefused(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "x")
	r := ScratchCleanupResult{Identity: ScratchIdentity{ID: "../../foreign", Root: root, Instance: "x"}, ArchivePath: t.TempDir(), Prepared: true}
	if err := scratchVerify(context.Background(), r); err == nil {
		t.Fatal("malformed journal identity accepted")
	}
	r.Identity.ID = "0123456789abcdef0123456789abcdef"
	if err := scratchVerify(context.Background(), r); err == nil {
		t.Fatal("arbitrary backup path accepted")
	}
}

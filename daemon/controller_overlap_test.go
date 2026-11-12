package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/worktree"
)

// FAE-CTL-001: the roll's overlap guard lists the files a new build changed,
// and the files each in-flight task touched, with a rename shown as both its
// paths and every name NUL-separated. So a task editing `a.go` is held when
// the build renames `a.go` to `b.go`, a task's own rename names its source
// path, and a path git would quote on a line (a quote or a non-ASCII byte in
// it) is listed as itself. On the base both lists came from `git diff
// --name-only` by line: a rename listed only its new name, quoted paths came
// back quoted, and the hold was missed.
func TestRollOverlapListsARenamedFilesBothNames(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		for _, v := range os.Environ() {
			if !strings.HasPrefix(v, "GIT_") {
				cmd.Env = append(cmd.Env, v)
			}
		}
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=Roll", "GIT_AUTHOR_EMAIL=roll@localhost", "GIT_COMMITTER_NAME=Roll", "GIT_COMMITTER_EMAIL=roll@localhost", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v %s", args, e, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	git("init", "-q", "-b", "main")
	write("\tlead-tab.go", "package p\n")
	write("a.go", "package p\n\n"+strings.Repeat("// line\n", 30))
	write("qu\"ote é.go", "package p\n")
	git("add", ".")
	git("commit", "-q", "-m", "old")
	old := git("rev-parse", "HEAD")
	write("\tlead-tab.go", "package p\n\n// edited\n")
	git("mv", "a.go", "b.go")
	write("b.go", "package p\n\n// edited\n"+strings.Repeat("// line\n", 30))
	write("qu\"ote é.go", "package p\n\n// edited\n")
	git("add", "-A")
	git("commit", "-q", "-m", "new")
	next := git("rev-parse", "HEAD")
	ctx := context.Background()
	changed, e := rollChangedNames(ctx, dir, old, next)
	if e != nil {
		t.Fatal(e)
	}
	slices.Sort(changed)
	if want := []string{"\tlead-tab.go", "a.go", "b.go", "qu\"ote é.go"}; !slices.Equal(changed, want) {
		t.Fatalf("the build's changed files: %q, want %q", changed, want)
	}
	// A task's worktree, as the guard reads it: the rename staged, not
	// committed, against the task's base.
	git("checkout", "-q", old)
	git("mv", "a.go", "c.go")
	write("c.go", "package p\n\n// task\n"+strings.Repeat("// line\n", 30))
	write(" lead.go", "package p\n")
	write("un\"tracked.go", "package p\n")
	touched, e := rollChangedNames(ctx, dir, old)
	if e != nil {
		t.Fatal(e)
	}
	slices.Sort(touched)
	if want := []string{"a.go", "c.go"}; !slices.Equal(touched, want) {
		t.Fatalf("the task's touched files: %q, want %q", touched, want)
	}
	untracked, e := rollNulNames(controller.Git(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z"))
	if e != nil || !slices.Equal(untracked, []string{" lead.go", "un\"tracked.go"}) {
		t.Fatalf("%q %v", untracked, e)
	}
	if _, e := rollChangedNames(ctx, dir, "no-such-revision"); e == nil {
		t.Fatal("a failed query must be an error, which the guard holds on")
	}

	// Drive Daemon.rollOverlap directly against a repository with a committed
	// rename (git mv old.go new.go) and in-flight task worktrees touching
	// either old.go or new.go.
	cfg, dCtx := fixture(t)
	ws := cfg.Workspace
	liveGit(t, ws, "init", "-b", "main")
	os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module example.invalid/rollrename\n\ngo 1.23\n"), 0600)
	os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0600)
	os.WriteFile(filepath.Join(ws, "old.go"), []byte("package main\n\n"+strings.Repeat("// old\n", 30)), 0600)
	os.WriteFile(filepath.Join(ws, "unrelated.go"), []byte("package main\n"), 0600)
	liveGit(t, ws, "add", ".")
	liveGit(t, ws, "commit", "-m", "base")
	baseSHA := liveGit(t, ws, "rev-parse", "HEAD")
	build := func(name string) string {
		t.Helper()
		out := filepath.Join(t.TempDir(), name)
		cmd := exec.Command("go", "build", "-buildvcs=true", "-o", out, ".")
		cmd.Dir = ws
		cmd.Env = append(cleanGitEnv(), "GOFLAGS=", "GOWORK=off")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v %s", name, err, b)
		}
		return out
	}
	running := build("fae-running")
	liveGit(t, ws, "mv", "old.go", "new.go")
	os.WriteFile(filepath.Join(ws, "new.go"), []byte("package main\n\n// renamed\n"+strings.Repeat("// old\n", 30)), 0600)
	liveGit(t, ws, "add", "-A")
	liveGit(t, ws, "commit", "-m", "rename old.go to new.go")
	targetSHA := liveGit(t, ws, "rev-parse", "HEAD")
	replacement := build("fae-replacement")
	liveGit(t, ws, "update-ref", "refs/fae/gated/main", targetSHA)

	d, e := Start(dCtx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	if e = d.recordGatedProof(dCtx, controlplane.Audit{Actor: "test", Outcome: "ok"}, GatedProof{
		Ref:   "refs/fae/gated/main",
		SHA:   targetSHA,
		Gates: []controller.GateResult{{Gate: "suite", Passed: true, Applicable: true, Count: 1}},
	}); e != nil {
		t.Fatal(e)
	}
	runningHash, e := controller.HashFile(running)
	if e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.binaryPath, d.binaryHash = running, runningHash
	mgr := &worktree.Manager{Repository: ws, Root: filepath.Join(t.TempDir(), "worktrees")}
	d.live = &liveExecutor{daemon: d, config: LiveConfig{Base: baseSHA}, manager: mgr}
	addTask := func(id, file, content string) {
		t.Helper()
		b, err := mgr.Ensure(dCtx, "run-"+id, id, baseSHA)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b.Path, file), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		job, err := engine.New(id, 1, defaults.Workflow(), b.Path)
		if err != nil {
			t.Fatal(err)
		}
		job.Base = baseSHA
		job.State = domain.State(job.Document.Start)
		raw, _ := json.Marshal(envelope{Job: job, Admission: AdmitParams{ID: id, RunID: "run-" + id}})
		d.jobs[id] = controlplane.JobSnapshot{ID: id, Payload: raw}
	}
	addTask("touches-old", "old.go", "package main\n\n// task edit\n")
	addTask("touches-new", "new.go", "package main\n\n// task new file\n")
	addTask("unrelated", "unrelated.go", "package main\n\n// unrelated edit\n")

	var h controller.Health
	if e := d.rollOverlap(dCtx, replacement, "refs/fae/gated/main", &h); e != nil {
		t.Fatal(e)
	}
	slices.Sort(h.Overlap)
	if want := []string{"touches-new", "touches-old"}; !slices.Equal(h.Overlap, want) {
		t.Fatalf("d.rollOverlap Overlap = %q, want %q", h.Overlap, want)
	}
}

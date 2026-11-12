package effects

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCommitIndexGuard(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			if CanReconcileCommitIndex(a, b) != (a && b) {
				t.Fatal(a, b)
			}
		}
	}
}
func TestCommitIndexCleanAndRecovery(t *testing.T) {
	for _, mode := range []string{"base-index", "reviewed-index", "after-cas"} {
		t.Run(mode, func(t *testing.T) {
			b, w, _ := setup(t)
			ctx := context.Background()
			write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
			write(t, w.Path, "new_test.go", "package test\n")
			snapshot, err := b.Snapshot(ctx, w, []string{"x.go", "new_test.go"})
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := b.PrepareCommit(ctx, w, snapshot, "reviewed commit")
			if err != nil {
				t.Fatal(err)
			}
			original, _ := os.ReadFile(filepath.Join(w.Path, "x.go"))
			if mode == "reviewed-index" {
				gitTest(t, w.Path, "add", "x.go", "new_test.go")
			}
			if mode == "after-cas" {
				gitTest(t, w.Path, "update-ref", "refs/heads/"+w.Branch, prepared.SHA, snapshot.Base)
				err = b.ReconcileCommitIndex(ctx, w, prepared)
			} else {
				_, err = b.ApplyCommit(ctx, w, prepared)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := gitTest(t, w.Path, "status", "--porcelain=v1"); got != "" {
				t.Fatal("committed workspace dirty", got)
			}
			after, _ := os.ReadFile(filepath.Join(w.Path, "x.go"))
			if string(after) != string(original) {
				t.Fatal("working bytes changed")
			}
			if err = b.ReconcileCommitIndex(ctx, w, prepared); err != nil {
				t.Fatal("idempotent reconciliation", err)
			}
		})
	}
}
func TestCommitIndexForeignStagingAndLockPreserved(t *testing.T) {
	for _, mode := range []string{"foreign-staging", "index-lock", "index-symlink", "recovery-foreign", "recovery-worktree", "recovery-head"} {
		t.Run(mode, func(t *testing.T) {
			b, w, _ := setup(t)
			ctx := context.Background()
			write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
			s, err := b.Snapshot(ctx, w, []string{"x.go"})
			if err != nil {
				t.Fatal(err)
			}
			p, err := b.PrepareCommit(ctx, w, s, "reviewed commit")
			if err != nil {
				t.Fatal(err)
			}
			index := gitTest(t, w.Path, "rev-parse", "--path-format=absolute", "--git-path", "index")
			recoverMode := mode == "recovery-foreign" || mode == "recovery-worktree" || mode == "recovery-head"
			if recoverMode {
				gitTest(t, w.Path, "update-ref", "refs/heads/"+w.Branch, p.SHA, s.Base)
			}
			switch mode {
			case "foreign-staging", "recovery-foreign":
				write(t, w.Path, "x.go", "package test\nfunc X() int { return 99 }\n")
				gitTest(t, w.Path, "add", "x.go")
				write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
			case "index-lock":
				if err = os.WriteFile(index+".lock", []byte("another git writer"), 0600); err != nil {
					t.Fatal(err)
				}
			case "index-symlink":
				if err = os.Rename(index, index+".saved"); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(index+".saved", index); err != nil {
					t.Fatal(err)
				}
			case "recovery-worktree":
				write(t, w.Path, "x.go", "package test\nfunc X() int { return 77 }\n")
			case "recovery-head":
				gitTest(t, w.Path, "update-ref", "refs/heads/"+w.Branch, s.Base, p.SHA)
			}
			before, err := os.ReadFile(index)
			if err != nil {
				t.Fatal(err)
			}
			head := gitTest(t, w.Path, "rev-parse", "HEAD")
			if recoverMode {
				err = b.ReconcileCommitIndex(ctx, w, p)
			} else {
				_, err = b.ApplyCommit(ctx, w, p)
			}
			if err == nil {
				t.Fatal("unsafe reconciliation accepted")
			}
			after, err := os.ReadFile(index)
			if err != nil || string(before) != string(after) {
				t.Fatal("foreign index changed", err)
			}
			if gitTest(t, w.Path, "rev-parse", "HEAD") != head {
				t.Fatal("ref advanced on index refusal")
			}
			if mode == "index-lock" {
				raw, err := os.ReadFile(index + ".lock")
				if err != nil || string(raw) != "another git writer" {
					t.Fatal("other lock removed", err)
				}
			}
		})
	}
}

func TestCommitIndexCrashHelper(t *testing.T) {
	raw := os.Getenv("COMMIT_INDEX_CRASH")
	if raw == "" {
		return
	}
	var p struct{ Index, Path, Branch, SHA, Base string }
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.Index)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(p.Index+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lock.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = lock.Sync(); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	gitTest(t, p.Path, "update-ref", "refs/heads/"+p.Branch, p.SHA, p.Base)
	// Simulate process death inside the protected region after ref CAS but before
	// index installation. Unknown stale locks deliberately require operator repair.
	syscall.Kill(os.Getpid(), syscall.SIGKILL)
	t.Fatal("SIGKILL returned")
}
func TestCommitIndexCrashLockRefusesRecovery(t *testing.T) {
	b, w, _ := setup(t)
	ctx := context.Background()
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
	s, err := b.Snapshot(ctx, w, []string{"x.go"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := b.PrepareCommit(ctx, w, s, "reviewed commit")
	if err != nil {
		t.Fatal(err)
	}
	index := gitTest(t, w.Path, "rev-parse", "--path-format=absolute", "--git-path", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(struct{ Index, Path, Branch, SHA, Base string }{index, w.Path, w.Branch, p.SHA, s.Base})
	cmd := exec.Command(os.Args[0], "-test.run=^TestCommitIndexCrashHelper$")
	cmd.Env = append(os.Environ(), "COMMIT_INDEX_CRASH="+string(raw))
	if err = cmd.Run(); err == nil {
		t.Fatal("crash fixture survived")
	}
	if gitTest(t, w.Path, "rev-parse", "HEAD") != p.SHA {
		t.Fatal("crash did not reach ref CAS")
	}
	lockBefore, err := os.ReadFile(index + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.ReconcileCommitIndex(ctx, w, p); err == nil {
		t.Fatal("unknown dead lock removed")
	}
	lockAfter, err := os.ReadFile(index + ".lock")
	if err != nil || string(lockBefore) != string(lockAfter) {
		t.Fatal("stale lock modified", err)
	}
	after, err := os.ReadFile(index)
	if err != nil || string(before) != string(after) {
		t.Fatal("index changed after refusal", err)
	}
	if tree, err := b.Tree(ctx, w); err != nil || tree != s.Tree {
		t.Fatal("working files changed", tree, err)
	}
}

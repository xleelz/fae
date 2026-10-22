package effects

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/xleelz/fae/worktree"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func upstream(t *testing.T, b *Broker, repo, base, name, contents string) string {
	t.Helper()
	// Build upstream through an independent index, leaving the primary checkout intact.
	dir := t.TempDir()
	idx := dir + "/index"
	run := func(input string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(cleanEnv(nil), "GIT_INDEX_FILE="+idx, "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@localhost", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@localhost")
		cmd.Stdin = strings.NewReader(input)
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("upstream %v: %v %s", args, e, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("", "read-tree", base)
	blob := run(contents, "hash-object", "-w", "--stdin")
	run("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+name)
	tree := run("", "write-tree")
	sha := run("upstream\n", "commit-tree", tree, "-p", base)
	gitTest(t, repo, "update-ref", "refs/heads/fae/integration", sha)
	return sha
}
func TestReconcileUnrelatedUpstreamAndIdempotentApply(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
	s, e := b.Snapshot(ctx, w, []string{"x.go"})
	if e != nil {
		t.Fatal(e)
	}
	tip := upstream(t, b, repo, base, "README.md", "upstream only\n")
	r, e := b.PrepareReconcile(ctx, w, s, "refs/heads/fae/integration")
	if e != nil {
		t.Fatal(e)
	}
	if r.Base != tip || !r.Changed || !r.PatchIdentical || r.AlreadyContained {
		t.Fatalf("%+v", r)
	}
	for i := 0; i < 2; i++ {
		if e = b.ApplyReconcile(ctx, w, r); e != nil {
			t.Fatal(e)
		}
	}
	if gitTest(t, repo, "rev-parse", "HEAD") != base || gitTest(t, repo, "status", "--porcelain") != "" {
		t.Fatal("primary checkout mutated")
	}
	if got, _ := os.ReadFile(w.Path + "/README.md"); string(got) != "upstream only\n" {
		t.Fatal("upstream absent")
	}
	if got, _ := os.ReadFile(w.Path + "/x.go"); !strings.Contains(string(got), "return 2") {
		t.Fatal("own hunk lost")
	}
	if _, e = b.Commit(ctx, w, s, "stale review"); e == nil {
		t.Fatal("stale review permitted")
	}
	fresh, e := b.Snapshot(ctx, w, s.Files)
	if e != nil {
		t.Fatal(e)
	}
	c, e := b.Commit(ctx, w, fresh, "verified reconciliation")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = b.Integrate(ctx, w, c, r.Ref, tip); e != nil {
		t.Fatal(e)
	}
}
func TestReconcileConflictAndTipRaceRefuse(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
	s, _ := b.Snapshot(ctx, w, []string{"x.go"})
	tip := upstream(t, b, repo, base, "x.go", "package test\nfunc X() int { return 3 }\n")
	_, e := b.PrepareReconcile(ctx, w, s, "refs/heads/fae/integration")
	var conflict *ReconcileError
	if !errors.As(e, &conflict) || conflict.Kind != "conflicted" || len(conflict.Paths) != 1 || conflict.Paths[0] != "x.go" {
		t.Fatalf("%T %v", e, e)
	}
	if gitTest(t, repo, "rev-parse", "refs/heads/fae/integration") != tip || gitTest(t, w.Path, "rev-parse", "HEAD") != base {
		t.Fatal("conflict mutated refs")
	}
	gitTest(t, repo, "update-ref", "refs/heads/fae/integration", base)
	r, e := b.PrepareReconcile(ctx, w, s, "refs/heads/fae/integration")
	if e != nil {
		t.Fatal(e)
	}
	upstream(t, b, repo, base, "other", "other\n")
	if e = b.ApplyReconcile(ctx, w, r); e == nil {
		t.Fatal("raced tip accepted")
	}
}
func TestReconcileInterruptedApplyAndEditedTree(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
	s, _ := b.Snapshot(ctx, w, []string{"x.go"})
	upstream(t, b, repo, base, "other", "other\n")
	r, e := b.PrepareReconcile(ctx, w, s, "refs/heads/fae/integration")
	if e != nil {
		t.Fatal(e)
	}
	gitTest(t, w.Path, "update-ref", "refs/heads/"+w.Branch, r.Base, base)
	if e = b.ApplyReconcile(ctx, w, r); e != nil {
		t.Fatal("interrupted apply", e)
	}
	write(t, w.Path, "x.go", "genuine unreviewed edit\n")
	if e = b.ApplyReconcile(ctx, w, r); e == nil {
		t.Fatal("edit overwritten")
	}
}

func TestIntegrationBlobAndEmptyContainment(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			b, w, repo := setup(t)
			ctx := context.Background()
			base := gitTest(t, repo, "rev-parse", "HEAD")
			gitTest(t, repo, "branch", "fae/integration")
			contents := "package test\nfunc X() int { return 2 }\n"
			if !empty {
				write(t, w.Path, "x.go", contents)
			}
			s, e := b.Snapshot(ctx, w, []string{"x.go"})
			if e != nil {
				t.Fatal(e)
			}
			c, e := b.Commit(ctx, w, s, "task")
			if e != nil {
				t.Fatal(e)
			}
			tip := upstream(t, b, repo, base, "x.go", contents)
			tip = upstream(t, b, repo, tip, "other", "unrelated\n")
			r, e := b.Integrate(ctx, w, c, "refs/heads/fae/integration", base)
			if e != nil || !r.Landed || r.ContainedBy != tip {
				t.Fatalf("%+v %v", r, e)
			}
			if gitTest(t, repo, "rev-parse", "refs/heads/fae/integration") != tip {
				t.Fatal("contained ref moved")
			}
		})
	}
}
func TestIntegrationPatchContainment(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	contents := "package test\nfunc X() int { return 2 }\n"
	write(t, w.Path, "x.go", contents)
	s, _ := b.Snapshot(ctx, w, []string{"x.go"})
	c, e := b.Commit(ctx, w, s, "task")
	if e != nil {
		t.Fatal(e)
	}
	tip := upstream(t, b, repo, base, "x.go", contents)
	tip = upstream(t, b, repo, tip, "x.go", contents+"// later unrelated addition\n")
	r, e := b.Integrate(ctx, w, c, "refs/heads/fae/integration", base)
	if e != nil || r.ContainedBy != tip {
		t.Fatalf("%+v %v", r, e)
	}
}
func TestReconcileAlreadyUpstreamAndEvidenceTampering(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	contents := "package test\nfunc X() int { return 2 }\n"
	write(t, w.Path, "x.go", contents)
	s, _ := b.Snapshot(ctx, w, []string{"x.go"})
	upstream(t, b, repo, base, "x.go", contents)
	r, e := b.PrepareReconcile(ctx, w, s, "refs/heads/fae/integration")
	if e != nil || !r.AlreadyContained {
		t.Fatalf("%+v %v", r, e)
	}
	bad := r
	bad.Tree = s.Tree
	bad.Changed = false
	if e = b.ApplyReconcile(ctx, w, bad); e == nil {
		t.Fatal("tampered evidence accepted")
	}
	if e = b.ApplyReconcile(ctx, w, r); e != nil {
		t.Fatal(e)
	}
}
func TestReconcileSameFileDistinctHunks(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	original := "package test\n// first\n\n\n\n\n\n\n\n\nfunc X() int { return 1 }\n"
	write(t, w.Path, "x.go", original)
	s, _ := b.Snapshot(ctx, w, []string{"x.go"})
	c, e := b.Commit(ctx, w, s, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	gitTest(t, repo, "branch", "fae/integration", c.SHA)
	write(t, w.Path, "x.go", strings.Replace(original, "return 1", "return 2", 1))
	s, e = b.Snapshot(ctx, w, []string{"x.go"})
	if e != nil {
		t.Fatal(e)
	}
	upstream(t, b, repo, c.SHA, "x.go", strings.Replace(original, "first", "upstream", 1))
	r, e := b.PrepareReconcile(ctx, w, s, "refs/heads/fae/integration")
	if e != nil || !r.PatchIdentical {
		t.Fatalf("%+v %v", r, e)
	}
	if e = b.ApplyReconcile(ctx, w, r); e != nil {
		t.Fatal(e)
	}
}

func TestReconcileRejectsPartialOwnHunkLoss(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
	write(t, w.Path, "new.go", "package test\nconst New = true\n")
	s, e := b.Snapshot(ctx, w, []string{"x.go", "new.go"})
	if e != nil {
		t.Fatal(e)
	}
	upstream(t, b, repo, base, "x.go", "package test\nfunc X() int { return 2 }\n")
	_, e = b.PrepareReconcile(ctx, w, s, "refs/heads/fae/integration")
	var refused *ReconcileError
	if !errors.As(e, &refused) || refused.Kind != "own-hunk-loss" || len(refused.Paths) != 1 || refused.Paths[0] != "x.go" {
		t.Fatalf("expected named fail-closed missing own diff, got %v", e)
	}
}

func TestContainmentDerivesActualTouchedPaths(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
	s, _ := b.Snapshot(ctx, w, []string{"x.go"})
	c, e := b.Commit(ctx, w, s, "task")
	if e != nil {
		t.Fatal(e)
	}
	upstream(t, b, repo, base, "other", "different\n")
	c.Files = []string{"go.mod"}
	if _, e = b.Integrate(ctx, w, c, "refs/heads/fae/integration", base); e == nil {
		t.Fatal("caller file list forged blob containment")
	}
}

func TestReconcileCommittedAndUncommittedOwnWork(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
	s, _ := b.Snapshot(ctx, w, []string{"x.go"})
	if _, e := b.Commit(ctx, w, s, "own commit"); e != nil {
		t.Fatal(e)
	}
	write(t, w.Path, "new.go", "package test\nconst New = true\n")
	s, e := b.Snapshot(ctx, w, []string{"x.go", "new.go"})
	if e != nil {
		t.Fatal(e)
	}
	upstream(t, b, repo, base, "README.md", "upstream\n")
	r, e := b.PrepareReconcileFrom(ctx, w, s, base, "refs/heads/fae/integration")
	if e != nil || !r.PatchIdentical {
		t.Fatalf("%+v %v", r, e)
	}
	if e = b.ApplyReconcile(ctx, w, r); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"x.go", "new.go", "README.md"} {
		if _, e = os.Stat(w.Path + "/" + p); e != nil {
			t.Fatal(e)
		}
	}
	if got, _ := os.ReadFile(w.Path + "/x.go"); !strings.Contains(string(got), "return 2") {
		t.Fatal("committed own work lost")
	}
}

func TestRecordedHostCommandOwnership(t *testing.T) {
	b, w, _ := setup(t)
	b.ProcessRegistry = b.Manager.Root + "/.processes"
	b.ProcessOwner = "test-owner"
	b.Allowed = [][]string{{"printf", "%s", "literal;$(unchanged)"}}
	r, e := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Second})
	if e != nil || r.Output != "literal;$(unchanged)" {
		t.Fatalf("%+v %v", r, e)
	}
	entries, e := os.ReadDir(b.ProcessRegistry)
	if e != nil || len(entries) != 0 {
		t.Fatalf("ownership records not retired: %v %v", entries, e)
	}
}

func TestReconcilePreservesBinaryDeletionAndMode(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	if e := os.Remove(w.Path + "/x_test.go"); e != nil {
		t.Fatal(e)
	}
	binary := []byte{0, 1, 255, 10, 0, 3}
	if e := os.WriteFile(w.Path+"/asset.bin", binary, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(w.Path+"/x.go", 0755); e != nil {
		t.Fatal(e)
	}
	s, e := b.Snapshot(ctx, w, []string{"x_test.go", "asset.bin", "x.go"})
	if e != nil {
		t.Fatal(e)
	}
	upstream(t, b, repo, base, "upstream", "other\n")
	r, e := b.PrepareReconcile(ctx, w, s, "refs/heads/fae/integration")
	if e != nil || !r.PatchIdentical {
		t.Fatalf("%+v %v", r, e)
	}
	if e = b.ApplyReconcile(ctx, w, r); e != nil {
		t.Fatal(e)
	}
	if got, e := os.ReadFile(w.Path + "/asset.bin"); e != nil || !bytes.Equal(got, binary) {
		t.Fatal("binary lost", got, e)
	}
	if _, e := os.Stat(w.Path + "/x_test.go"); !os.IsNotExist(e) {
		t.Fatal("deletion lost", e)
	}
	if info, e := os.Stat(w.Path + "/x.go"); e != nil || info.Mode().Perm()&0111 == 0 {
		t.Fatal("mode change lost", e)
	}
}

func TestConcurrentIntegrationCASHasSingleWinner(t *testing.T) {
	b, first, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	second, e := b.Manager.Ensure(ctx, "run", "second", base)
	if e != nil {
		t.Fatal(e)
	}
	commits := make([]CommitEvidence, 2)
	trees := []worktree.Binding{first, second}
	for i, w := range trees {
		write(t, w.Path, "x.go", fmt.Sprintf("package test\nfunc X() int { return %d }\n", i+2))
		s, e := b.Snapshot(ctx, w, []string{"x.go"})
		if e != nil {
			t.Fatal(e)
		}
		commits[i], e = b.Commit(ctx, w, s, "concurrent task")
		if e != nil {
			t.Fatal(e)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, w := range trees {
		go func(i int, w worktree.Binding) {
			<-start
			_, e := b.Integrate(ctx, w, commits[i], "refs/heads/fae/integration", base)
			results <- e
		}(i, w)
	}
	close(start)
	winners := 0
	for range trees {
		if <-results == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d integration winners", winners)
	}
	tip := gitTest(t, repo, "rev-parse", "refs/heads/fae/integration")
	if tip != commits[0].SHA && tip != commits[1].SHA {
		t.Fatal("unknown landed commit")
	}
}

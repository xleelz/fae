package effects

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xleelz/fae/workflow"
)

func TestCheckCommandWritesExemptionOverlapRefusalAndNonCommit(t *testing.T) {
	b, w, _ := setup(t)
	ctx := context.Background()

	// Create a script inside tmp or use python3 / sh to write a check report file
	jsonCmd := []string{"python3", "-c", `import os; os.makedirs("build/check", exist_ok=True); open("build/check/out.json","w").write('{"models_checked": 3, "status": "ok"}')`}
	linesCmd := []string{"python3", "-c", `print("model/a.yaml\nmodel/b.yaml\n")`}
	b.Allowed = [][]string{jsonCmd, linesCmd}

	// 1. Overlap between Check.Writes and DeclaredFiles is refused before execution.
	_, err := b.Run(ctx, w, Command{
		Argv:          jsonCmd,
		Timeout:       10 * time.Second,
		DeclaredFiles: []string{"build/check/out.json", "x.go"},
		Check: &workflow.CheckSpec{
			Format:     "json",
			Output:     "build/check/out.json",
			CountField: "models_checked",
			Writes:     []string{"build/check/**"},
		},
	})
	var refused *RefusedBeforeExec
	if err == nil || !errors.As(err, &refused) {
		t.Fatalf("expected RefusedBeforeExec when Check.Writes overlaps DeclaredFiles, got: %v", err)
	}

	// 2. Disjoint Check.Writes succeeds, parses output file, exempts Writes from mutation error,
	// and retains Check.Writes as scratch readable by later stages, excluded from commits.
	b.Scratch = []string{"build/check/**"}
	beforeTree, err := b.Tree(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Run(ctx, w, Command{
		Argv:          jsonCmd,
		Timeout:       10 * time.Second,
		DeclaredFiles: []string{"x.go"},
		Check: &workflow.CheckSpec{
			Format:     "json",
			Output:     "build/check/out.json",
			CountField: "models_checked",
			Writes:     []string{"build/check/**"},
		},
	})
	if err != nil {
		t.Fatalf("expected check command with disjoint Writes to succeed, got: %v", err)
	}
	if res.CheckCount != 3 || res.CheckPayload["models_checked"] == nil {
		t.Fatalf("expected CheckCount=3, got count=%v payload=%v", res.CheckCount, res.CheckPayload)
	}
	if data, statErr := os.ReadFile(filepath.Join(w.Path, "build/check/out.json")); statErr != nil || len(data) == 0 {
		t.Fatalf("check scratch not retained: %s %v", data, statErr)
	}
	afterTree, err := b.Tree(ctx, w)
	if err != nil || afterTree != beforeTree {
		t.Fatalf("expected worktree tree to be unchanged (%s), got %s (err=%v)", beforeTree, afterTree, err)
	}

	// 3. Lines format check command on stdout populates count_field and lines.
	lRes, err := b.Run(ctx, w, Command{
		Argv:          linesCmd,
		Timeout:       10 * time.Second,
		DeclaredFiles: []string{"x.go"},
		Check: &workflow.CheckSpec{
			Format:     "lines",
			Output:     "stdout",
			CountField: "checked_lines",
		},
	})
	if err != nil {
		t.Fatalf("expected lines check command to succeed, got: %v", err)
	}
	if lRes.CheckCount != 2 || lRes.CheckPayload["checked_lines"] != 2 {
		t.Fatalf("expected CheckCount=2, got count=%v payload=%v", lRes.CheckCount, lRes.CheckPayload)
	}
}

func TestBranchWorktreesForkCommitScopeAndOrderedFanInIntegration(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()

	// Create two initial package directories in the repo and update worktree.
	if err := os.MkdirAll(filepath.Join(repo, "pkg/alpha"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "pkg/beta"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "pkg/alpha/a.go", "package alpha\nfunc A() int { return 1 }\n")
	write(t, repo, "pkg/beta/b.go", "package beta\nfunc B() int { return 1 }\n")
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "add alpha and beta")
	headSHA := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, w.Path, "reset", "--hard", headSHA)

	// Fork branch-0 (pkg/alpha/**) and branch-1 (pkg/beta/**).
	bw0, err := b.ForkBranchWorktree(ctx, w, "fan", "branch-0", []string{"pkg/alpha/**"})
	if err != nil {
		t.Fatalf("ForkBranchWorktree branch-0 failed: %v", err)
	}
	defer b.CleanupBranchWorktree(ctx, bw0)

	bw1, err := b.ForkBranchWorktree(ctx, w, "fan", "branch-1", []string{"pkg/beta/**"})
	if err != nil {
		t.Fatalf("ForkBranchWorktree branch-1 failed: %v", err)
	}
	defer b.CleanupBranchWorktree(ctx, bw1)

	// Branch 0 modifies pkg/alpha/a.go (allowed).
	write(t, bw0.Binding.Path, "pkg/alpha/a.go", "package alpha\nfunc A() int { return 10 }\n")
	bw0, err = b.CommitBranchWorktree(ctx, bw0, []string{"pkg/alpha/**", "pkg/beta/**"}, "branch-0 commit")
	if err != nil {
		t.Fatalf("CommitBranchWorktree branch-0 failed: %v", err)
	}

	// Branch 1 attempts to modify pkg/alpha/a.go (outside its write_scope pkg/beta/**) -> must fail!
	write(t, bw1.Binding.Path, "pkg/alpha/a.go", "package alpha\nfunc A() int { return 99 }\n")
	if _, err := b.CommitBranchWorktree(ctx, bw1, []string{"pkg/alpha/**", "pkg/beta/**"}, "branch-1 out-of-scope"); err == nil {
		t.Fatal("expected CommitBranchWorktree to refuse modification outside branch write_scope")
	}
	// Reset out-of-scope edit and modify pkg/beta/b.go (in scope).
	gitTest(t, bw1.Binding.Path, "checkout", "--", "pkg/alpha/a.go")
	write(t, bw1.Binding.Path, "pkg/beta/b.go", "package beta\nfunc B() int { return 20 }\n")
	bw1, err = b.CommitBranchWorktree(ctx, bw1, []string{"pkg/alpha/**", "pkg/beta/**"}, "branch-1 commit")
	if err != nil {
		t.Fatalf("CommitBranchWorktree branch-1 failed: %v", err)
	}

	// Integrate branch-0 and branch-1 in deterministic order [0, 1] into parent worktree w.
	mergedTree, err := b.IntegrateBranchWorktrees(ctx, w, []BranchWorktree{bw0, bw1})
	if err != nil {
		t.Fatalf("IntegrateBranchWorktrees failed: %v", err)
	}
	if mergedTree == "" {
		t.Fatalf("expected non-empty mergedTree, got %q", mergedTree)
	}
	gotA, err := os.ReadFile(filepath.Join(w.Path, "pkg/alpha/a.go"))
	if err != nil || string(gotA) != "package alpha\nfunc A() int { return 10 }\n" {
		t.Fatalf("unexpected pkg/alpha/a.go after FanIn integration: %q (%v)", string(gotA), err)
	}
	gotB, err := os.ReadFile(filepath.Join(w.Path, "pkg/beta/b.go"))
	if err != nil || string(gotB) != "package beta\nfunc B() int { return 20 }\n" {
		t.Fatalf("unexpected pkg/beta/b.go after FanIn integration: %q (%v)", string(gotB), err)
	}
}

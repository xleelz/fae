package effects

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/worktree"
)

func TestConcurrentLandingReconcilesCommittedLoser(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	other, err := b.Manager.Ensure(ctx, "run", "other", base)
	if err != nil {
		t.Fatal(err)
	}
	trees := []worktree.Binding{w, other}
	names := []string{"alpha.go", "beta.go"}
	commits := make([]CommitEvidence, 2)
	for i, tree := range trees {
		write(t, tree.Path, names[i], "package test\nconst "+strings.ToUpper(names[i][:1])+" = true\n")
		s, e := b.Snapshot(ctx, tree, []string{names[i]})
		if e != nil {
			t.Fatal(e)
		}
		commits[i], e = b.Commit(ctx, tree, s, "reviewed concurrent change")
		if e != nil {
			t.Fatal(e)
		}
	}
	type outcome struct {
		index int
		err   error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for i, tree := range trees {
		go func(i int, tree worktree.Binding) {
			<-start
			_, err := b.Integrate(ctx, tree, commits[i], "refs/heads/fae/integration", base)
			results <- outcome{i, err}
		}(i, tree)
	}
	close(start)
	winner, loser := -1, -1
	for range trees {
		r := <-results
		if r.err == nil {
			if winner != -1 {
				t.Fatal("two winners")
			}
			winner = r.index
		} else {
			var moved *TargetMovedError
			if !errors.As(r.err, &moved) || !moved.NoWrite || !moved.OwnCommitKnown || moved.Actual == moved.Expected {
				t.Fatalf("unproven no-write retry: %v", r.err)
			}
			loser = r.index
		}
	}
	if winner < 0 || loser < 0 {
		t.Fatal("missing winner or loser")
	}
	tree := trees[loser]
	snapshot, err := b.Snapshot(ctx, tree, []string{names[loser]})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Base != commits[loser].SHA {
		t.Fatal("test did not preserve committed loser")
	}
	plan, err := b.PrepareReconcileFrom(ctx, tree, snapshot, base, "refs/heads/fae/integration")
	if err != nil || !plan.PatchIdentical {
		t.Fatalf("%+v %v", plan, err)
	}
	if err = b.ApplyReconcile(ctx, tree, plan); err != nil {
		t.Fatal(err)
	}
	b.Allowed = [][]string{{"go", "test", "-json", "-count=1", "-a", "-x", "./..."}}
	run, err := b.Run(ctx, tree, Command{Argv: b.Allowed[0], Timeout: time.Minute, TestID: "TestX"})
	if err != nil || run.ExitCode != 0 || run.ExecutedCount != 1 || !run.FreshBuild || run.Tree != plan.Tree {
		t.Fatalf("reconciled tree not freshly verified: %+v %v", run, err)
	}
	snapshot, err = b.Snapshot(ctx, tree, []string{names[loser]})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := b.Commit(ctx, tree, snapshot, "freshly verified reconciled change")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Integrate(ctx, tree, commit, "refs/heads/fae/integration", plan.Base); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if !strings.Contains(gitTest(t, repo, "show", commit.SHA+":"+name), "= true") {
			t.Fatal("concurrent own delta lost", name)
		}
	}
	if gitTest(t, repo, "rev-parse", "HEAD") != base || gitTest(t, repo, "status", "--porcelain") != "" {
		t.Fatal("primary changed")
	}
}

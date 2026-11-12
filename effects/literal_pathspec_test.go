package effects

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// literalPathspecNames are file names a task may add (each passes the
// declared-path rule, the protected-path rule and the relative-path argument
// check) that git reads as pathspec magic unless it is told to take paths
// literally.
var literalPathspecNames = []string{":(exclude)*", ":!*", ":(exclude)x.go"}

// FAE-SEC-005, FAE-VCS-004/-007: Integrate's containment check compares the
// files the task commit changed, by name, with the integration tip. Before,
// those names reached `git diff` as pathspecs: a task commit that also added
// a file named ":(exclude)*" excluded every file from the comparison, so a
// commit the tip never received was "already contained" and Integrate
// returned Landed:true while the ref did not move. Now the names are literal:
// the commit is not contained, and the moved base is reported.
func TestIntegrateTakesTaskFileNamesLiterally(t *testing.T) {
	for _, name := range literalPathspecNames {
		t.Run(name, func(t *testing.T) {
			b, w, repo := setup(t)
			ctx := context.Background()
			base := gitTest(t, repo, "rev-parse", "HEAD")
			gitTest(t, repo, "branch", "fae/integration")
			write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
			write(t, w.Path, name, "the task's own file\n")
			s, err := b.Snapshot(ctx, w, []string{"x.go", name})
			if err != nil {
				t.Fatal(err)
			}
			c, err := b.Commit(ctx, w, s, "task")
			if err != nil {
				t.Fatal(err)
			}
			tip := upstream(t, b, repo, base, "README.md", "upstream only\n")
			ev, err := b.Integrate(ctx, w, c, "refs/heads/fae/integration", base)
			var moved *TargetMovedError
			if ev.Landed || !errors.As(err, &moved) || moved.Actual != tip {
				t.Fatalf("a commit the tip does not hold was integrated: %+v %v", ev, err)
			}
			if now := gitTest(t, repo, "rev-parse", "refs/heads/fae/integration"); now != tip {
				t.Fatalf("integration ref moved to %s", now)
			}
		})
	}
	// Literal names still find containment: a tip that already holds the
	// commit's exact files contains it.
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	write(t, w.Path, ":(exclude)*", "the task's own file\n")
	s, err := b.Snapshot(ctx, w, []string{":(exclude)*"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Commit(ctx, w, s, "task")
	if err != nil {
		t.Fatal(err)
	}
	tip := upstream(t, b, repo, base, ":(exclude)*", "the task's own file\n")
	ev, err := b.Integrate(ctx, w, c, "refs/heads/fae/integration", base)
	if err != nil || !ev.Landed || ev.Containment != ContainedUpstream || ev.ContainedBy != tip {
		t.Fatalf("an upstream holding the commit's files byte for byte does not contain it: %+v %v", ev, err)
	}
}

// FAE-SEC-005, FAE-VCS-004: a reconcile's "already contained" verdict, which
// waives the own-hunk-loss check, compared the task's changed files by name as
// pathspecs. With a task file named ":(exclude)*" the comparison matched no
// file, so an upstream holding none of the task's work "already contained"
// it. Now the names are literal.
func TestReconcileTakesTaskFileNamesLiterally(t *testing.T) {
	for _, name := range literalPathspecNames {
		t.Run(name, func(t *testing.T) {
			b, w, repo := setup(t)
			ctx := context.Background()
			base := gitTest(t, repo, "rev-parse", "HEAD")
			gitTest(t, repo, "branch", "fae/integration")
			write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
			write(t, w.Path, name, "the task's own file\n")
			s, err := b.Snapshot(ctx, w, []string{"x.go", name})
			if err != nil {
				t.Fatal(err)
			}
			upstream(t, b, repo, base, "README.md", "upstream only\n")
			r, err := b.PrepareReconcile(ctx, w, s, "refs/heads/fae/integration")
			if err != nil {
				t.Fatal(err)
			}
			if r.AlreadyContained || !r.PatchIdentical {
				t.Fatalf("an upstream without the task's work contains it: %+v", r)
			}
		})
	}
}

// FAE-SEC-005, FAE-VCS-004: a conflict resolution is checked file by file
// against the task's own hunks. Before, a task file named ":(exclude)*" was
// compared as a pathspec that matched nothing, so it counted as "upstream
// holds it and the resolution keeps it" and was skipped: a resolution that
// threw its content away passed. Now it is refused, naming the file.
func TestConflictResolutionTakesTaskFileNamesLiterally(t *testing.T) {
	for _, name := range literalPathspecNames {
		t.Run(name, func(t *testing.T) {
			b, w, repo := setup(t)
			ctx := context.Background()
			base := gitTest(t, repo, "rev-parse", "HEAD")
			gitTest(t, repo, "branch", "fae/integration")
			own := "package test\nfunc X() int { return 2 }\n"
			write(t, w.Path, "x.go", own)
			write(t, w.Path, name, "the task's own line\n")
			s, err := b.Snapshot(ctx, w, []string{"x.go", name})
			if err != nil {
				t.Fatal(err)
			}
			upstream(t, b, repo, base, "x.go", "package test\nfunc X() int { return 3 }\n")
			plan, err := b.PrepareReconcile(ctx, w, s, "refs/heads/fae/integration")
			var conflict *ReconcileError
			if !errors.As(err, &conflict) || !slices.Equal(conflict.Paths, []string{"x.go"}) {
				t.Fatalf("%+v %v", plan, err)
			}
			if err = b.ApplyConflict(ctx, w, plan, conflict.Paths); err != nil {
				t.Fatal(err)
			}
			write(t, w.Path, "x.go", own)
			if err = b.VerifyConflictResolution(ctx, w, plan); err != nil {
				t.Fatal("a resolution keeping every own hunk refused:", err)
			}
			write(t, w.Path, name, "another line\n")
			if err = b.VerifyConflictResolution(ctx, w, plan); !errors.As(err, &conflict) || conflict.Kind != "own-hunk-loss" || !slices.Equal(conflict.Paths, []string{name}) {
				t.Fatalf("a resolution dropping %q's own hunk passed: %v", name, err)
			}
			if err = os.Remove(filepath.Join(w.Path, name)); err != nil {
				t.Fatal(err)
			}
			if err = b.VerifyConflictResolution(ctx, w, plan); !errors.As(err, &conflict) || conflict.Kind != "own-hunk-loss" || !slices.Equal(conflict.Paths, []string{name}) {
				t.Fatalf("a resolution deleting %q passed: %v", name, err)
			}
		})
	}
}

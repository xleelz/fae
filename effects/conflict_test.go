package effects

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestConflictPlanResolutionPreservesOriginalHunks(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	own := "package test\nfunc X() int { return 2 }\n"
	write(t, w.Path, "x.go", own)
	s, e := b.Snapshot(ctx, w, []string{"x.go"})
	if e != nil {
		t.Fatal(e)
	}
	tip := upstream(t, b, repo, base, "x.go", "package test\nfunc X() int { return 3 }\n")
	plan, e := b.PrepareReconcile(ctx, w, s, "refs/heads/fae/integration")
	var conflict *ReconcileError
	if !errors.As(e, &conflict) || plan.Tree == "" {
		t.Fatalf("%+v %v", plan, e)
	}
	if e = b.ApplyConflict(ctx, w, plan, conflict.Paths); e != nil {
		t.Fatal(e)
	}
	if e = b.VerifyConflictResolution(ctx, w, plan); e == nil {
		t.Fatal("unresolved markers accepted")
	}
	write(t, w.Path, "x.go", own)
	if e = b.VerifyConflictResolution(ctx, w, plan); e != nil {
		t.Fatal(e)
	}
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 3 }\n")
	if e = b.VerifyConflictResolution(ctx, w, plan); !errors.As(e, &conflict) || conflict.Kind != "own-hunk-loss" || len(conflict.Hunks) != 1 || !strings.Contains(conflict.Hunks[0], "@@") {
		t.Fatalf("missing named hunk refusal: %v", e)
	}
	if gitTest(t, repo, "rev-parse", "refs/heads/fae/integration") != tip || gitTest(t, repo, "rev-parse", "HEAD") != base {
		t.Fatal("unverified resolution landed")
	}
}
func TestConflictPlanRefusesForgeryAndPreservesIndependentFile(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	own := "package test\nfunc X() int { return 2 }\n"
	write(t, w.Path, "x.go", own)
	write(t, w.Path, "new.go", "package test\nconst Own = true\n")
	s, _ := b.Snapshot(ctx, w, []string{"x.go", "new.go"})
	upstream(t, b, repo, base, "x.go", "package test\nfunc X() int { return 3 }\n")
	plan, e := b.PrepareReconcile(ctx, w, s, "refs/heads/fae/integration")
	var conflict *ReconcileError
	if !errors.As(e, &conflict) {
		t.Fatal(e)
	}
	if e = b.ApplyConflict(ctx, w, plan, []string{"new.go"}); e == nil {
		t.Fatal("forged conflict paths accepted")
	}
	forged := plan
	forged.Tree = plan.OriginalTree
	if e = b.ApplyConflict(ctx, w, forged, conflict.Paths); e == nil {
		t.Fatal("forged tree accepted")
	}
	if e = b.ApplyConflict(ctx, w, plan, conflict.Paths); e != nil {
		t.Fatal(e)
	}
	write(t, w.Path, "x.go", own)
	write(t, w.Path, "new.go", "package test\n")
	if e = b.VerifyConflictResolution(ctx, w, plan); !errors.As(e, &conflict) || len(conflict.Paths) != 1 || conflict.Paths[0] != "new.go" {
		t.Fatalf("lost independent own hunk not identified: %v", e)
	}
	if err := os.Remove(w.Path + "/new.go"); err != nil {
		t.Fatal(err)
	}
	if e = b.VerifyConflictResolution(ctx, w, plan); !errors.As(e, &conflict) || len(conflict.Paths) != 1 || conflict.Paths[0] != "new.go" || len(conflict.Hunks) == 0 {
		t.Fatalf("lost file not identified: %v", e)
	}
}

// FAE-VCS-004/-007: inside a conflicted reconcile, a file the task changed
// that upstream already holds byte-identically is contained, not a lost hunk;
// a later edit to that file is still refused, naming it.
func TestConflictResolutionAcceptsContainedFile(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	own, added := "package test\nfunc X() int { return 2 }\n", "package test\nconst Own = true\n"
	write(t, w.Path, "x.go", own)
	write(t, w.Path, "new.go", added)
	s, e := b.Snapshot(ctx, w, []string{"x.go", "new.go"})
	if e != nil {
		t.Fatal(e)
	}
	tip := upstream(t, b, repo, base, "new.go", added)
	upstream(t, b, repo, tip, "x.go", "package test\nfunc X() int { return 3 }\n")
	plan, e := b.PrepareReconcile(ctx, w, s, "refs/heads/fae/integration")
	var conflict *ReconcileError
	if !errors.As(e, &conflict) || strings.Join(conflict.Paths, ",") != "x.go" {
		t.Fatalf("%+v %v", plan, e)
	}
	if e = b.ApplyConflict(ctx, w, plan, conflict.Paths); e != nil {
		t.Fatal(e)
	}
	write(t, w.Path, "x.go", own)
	if e = b.VerifyConflictResolution(ctx, w, plan); e != nil {
		t.Fatal("contained file reported lost:", e)
	}
	write(t, w.Path, "new.go", "package test\n")
	if e = b.VerifyConflictResolution(ctx, w, plan); !errors.As(e, &conflict) || strings.Join(conflict.Paths, ",") != "new.go" {
		t.Fatalf("edited contained file not refused: %v", e)
	}
}

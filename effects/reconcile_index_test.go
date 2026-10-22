package effects

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestReconcileIndexConflictResolutionCommit(t *testing.T) {
	for _, mode := range []string{"fresh", "after-ref", "legacy-conflict-index", "foreign-staging", "held-lock"} {
		t.Run(mode, func(t *testing.T) {
			b, w, repo := setup(t)
			ctx := context.Background()
			base := gitTest(t, repo, "rev-parse", "HEAD")
			gitTest(t, repo, "branch", "fae/integration")
			own := "package test\nfunc X() int { return 2 }\n"
			write(t, w.Path, "x.go", own)
			snapshot, err := b.Snapshot(ctx, w, []string{"x.go"})
			if err != nil {
				t.Fatal(err)
			}
			upstream(t, b, repo, base, "x.go", "package test\nfunc X() int { return 3 }\n")
			plan, err := b.PrepareReconcile(ctx, w, snapshot, "refs/heads/fae/integration")
			var conflict *ReconcileError
			if !errors.As(err, &conflict) {
				t.Fatal(err)
			}
			index := gitTest(t, w.Path, "rev-parse", "--path-format=absolute", "--git-path", "index")
			switch mode {
			case "after-ref":
				gitTest(t, w.Path, "update-ref", "refs/heads/"+w.Branch, plan.Base, base)
			case "legacy-conflict-index":
				gitTest(t, w.Path, "update-ref", "refs/heads/"+w.Branch, plan.Base, base)
				gitTest(t, w.Path, "read-tree", "--reset", "-u", plan.Tree)
			case "foreign-staging":
				write(t, w.Path, "x.go", "package test\nfunc X() int { return 99 }\n")
				gitTest(t, w.Path, "add", "x.go")
				write(t, w.Path, "x.go", own)
			case "held-lock":
				if err = os.WriteFile(index+".lock", []byte("other owner"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(index)
			if err != nil {
				t.Fatal(err)
			}
			err = b.ApplyConflict(ctx, w, plan, conflict.Paths)
			if mode == "foreign-staging" || mode == "held-lock" {
				if err == nil {
					t.Fatal("unsafe reconciliation accepted")
				}
				after, _ := os.ReadFile(index)
				if string(before) != string(after) || gitTest(t, w.Path, "rev-parse", "HEAD") != base {
					t.Fatal("refusal changed index or head")
				}
				bytes, _ := os.ReadFile(w.Path + "/x.go")
				if string(bytes) != own {
					t.Fatal("refusal changed working files")
				}
				if mode == "held-lock" {
					v, _ := os.ReadFile(index + ".lock")
					if string(v) != "other owner" {
						t.Fatal("foreign lock changed")
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := gitTest(t, w.Path, "write-tree"); got != gitTest(t, w.Path, "rev-parse", plan.Base+"^{tree}") {
				t.Fatal("reconcile index not based on new HEAD")
			}
			if err = b.ApplyConflict(ctx, w, plan, conflict.Paths); err != nil {
				t.Fatal("retry", err)
			}
			write(t, w.Path, "x.go", own)
			if err = b.VerifyConflictResolution(ctx, w, plan); err != nil {
				t.Fatal(err)
			}
			fresh, err := b.Snapshot(ctx, w, []string{"x.go"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = b.Commit(ctx, w, fresh, "resolved and reviewed"); err != nil {
				t.Fatal(err)
			}
			if got := gitTest(t, w.Path, "status", "--porcelain=v1"); got != "" {
				t.Fatal("dirty resolved commit", got)
			}
		})
	}
}

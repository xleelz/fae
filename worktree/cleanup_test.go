package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func cleanupFixture(t *testing.T) (*Manager, CleanupPlan) {
	t.Helper()
	m := manager(t)
	ctx := context.Background()
	b, e := m.Ensure(ctx, "run", "task", "HEAD")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = git(ctx, b.Path, "commit", "--allow-empty", "-m", "own commit"); e != nil {
		t.Fatal(e)
	}
	p, e := m.PlanCleanup(ctx, "run", "task")
	if e != nil {
		t.Fatal(e)
	}
	return m, p
}
func TestCleanupArchivesEveryCommitAndRepeats(t *testing.T) {
	m, p := cleanupFixture(t)
	ctx := context.Background()
	if e := m.Cleanup(ctx, p, func(context.Context) error { return nil }); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(p.Binding.Path); !os.IsNotExist(e) {
		t.Fatal("worktree retained", e)
	}
	archived, e := git(ctx, m.Repository, "rev-parse", p.Archive)
	if e != nil || archived != p.Head {
		t.Fatal(archived, e)
	}
	if _, e = git(ctx, m.Repository, "merge-base", "--is-ancestor", p.Head, p.Archive); e != nil {
		t.Fatal(e)
	}
	again, e := m.PlanCleanup(ctx, "run", "task")
	if e != nil || !again.AlreadyRemoved {
		t.Fatal(again, e)
	}
	if e = m.Cleanup(ctx, again, func(context.Context) error { return nil }); e != nil {
		t.Fatal(e)
	}
}
func TestCleanupFailuresPreserveWorktreeAndBranch(t *testing.T) {
	for _, kind := range []string{"backup", "archive", "dirty", "ignored", "branch-drift", "foreign", "late-backup", "late-dirty", "archive-drift"} {
		t.Run(kind, func(t *testing.T) {
			m, p := cleanupFixture(t)
			ctx := context.Background()
			check := func(context.Context) error { return nil }
			switch kind {
			case "backup":
				check = func(context.Context) error { return errors.New("backup failed") }
			case "archive":
				common, e := git(ctx, m.Repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
				if e != nil {
					t.Fatal(e)
				}
				if e = os.MkdirAll(filepath.Join(common, "refs", "fae"), 0700); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(filepath.Join(common, "refs", "fae", "archive"), []byte("blocked"), 0600); e != nil {
					t.Fatal(e)
				}
			case "dirty":
				os.WriteFile(filepath.Join(p.Binding.Path, "untracked"), []byte("sole copy"), 0600)
			case "ignored":
				os.WriteFile(filepath.Join(p.Binding.Path, ".gitignore"), []byte("ignored\n"), 0600)
				git(ctx, p.Binding.Path, "add", ".gitignore")
				git(ctx, p.Binding.Path, "commit", "-m", "ignore")
				p, _ = m.PlanCleanup(ctx, "run", "task")
				os.WriteFile(filepath.Join(p.Binding.Path, "ignored"), []byte("sole copy"), 0600)
			case "branch-drift":
				git(ctx, p.Binding.Path, "commit", "--allow-empty", "-m", "new")
			case "foreign":
				p.Binding.Path = m.Repository
			case "late-backup":
				n := 0
				check = func(context.Context) error {
					n++
					if n == 2 {
						return errors.New("backup lost")
					}
					return nil
				}
			case "late-dirty":
				check = func(context.Context) error {
					return os.WriteFile(filepath.Join(p.Binding.Path, "late"), []byte("sole copy"), 0600)
				}
			case "archive-drift":
				base, _ := git(ctx, m.Repository, "rev-parse", "HEAD")
				git(ctx, m.Repository, "update-ref", p.Archive, base)
			}
			if e := m.Cleanup(ctx, p, check); e == nil {
				t.Fatal("unsafe cleanup accepted")
			}
			binding := m.Binding("run", "task")
			if _, e := os.Stat(binding.Path); e != nil {
				t.Fatal("worktree deleted", e)
			}
			if _, e := git(ctx, m.Repository, "rev-parse", "refs/heads/"+binding.Branch); e != nil {
				t.Fatal("branch deleted", e)
			}
		})
	}
}
func TestCleanupUnownedAndMissingRefRefused(t *testing.T) {
	m, p := cleanupFixture(t)
	ctx := context.Background()
	git(ctx, m.Repository, "worktree", "remove", p.Binding.Path)
	git(ctx, m.Repository, "update-ref", "-d", "refs/heads/"+p.Binding.Branch)
	if _, e := m.PlanCleanup(ctx, "run", "task"); e == nil {
		t.Fatal("missing archive accepted")
	}
	if e := m.Cleanup(ctx, p, nil); e == nil {
		t.Fatal("missing backup verifier accepted")
	}
}

func TestCleanupAllArchivesBeforeDeletion(t *testing.T) {
	m, p := cleanupFixture(t)
	ctx := context.Background()
	b, e := m.Ensure(ctx, "run", "second", "HEAD")
	if e != nil {
		t.Fatal(e)
	}
	second, e := m.PlanCleanup(ctx, "run", "second")
	if e != nil {
		t.Fatal(e)
	}
	common, e := git(ctx, m.Repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if e != nil {
		t.Fatal(e)
	}
	parent := filepath.Join(common, "refs", "fae", "archive")
	if e = os.MkdirAll(parent, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(parent, Identity("run", "second")), []byte("blocked"), 0600); e != nil {
		t.Fatal(e)
	}
	check := func(context.Context) error { return nil }
	if e = m.ArchiveCleanup(ctx, p, check); e != nil {
		t.Fatal(e)
	}
	if e = m.ArchiveCleanup(ctx, second, check); e == nil {
		t.Fatal("archive collision accepted")
	}
	for _, binding := range []Binding{p.Binding, b} {
		if _, e = os.Stat(binding.Path); e != nil {
			t.Fatal("removed before all archives verified", e)
		}
	}
}
func TestCleanupGuardTruthTable(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			if CanCleanupArchived(a, b) != (a && b) {
				t.Fatal(a, b)
			}
		}
	}
}

func TestCleanupMovedForeignWorktreeRefused(t *testing.T) {
	m, p := cleanupFixture(t)
	ctx := context.Background()
	moved := filepath.Join(filepath.Dir(m.Root), "foreign")
	if _, e := git(ctx, m.Repository, "worktree", "move", p.Binding.Path, moved); e != nil {
		t.Fatal(e)
	}
	if _, e := m.PlanCleanup(ctx, "run", "task"); e == nil {
		t.Fatal("foreign registration ignored")
	}
	if _, e := os.Stat(moved); e != nil {
		t.Fatal(e)
	}
}

func TestCleanupSymbolicAliasesRefused(t *testing.T) {
	for _, kind := range []string{"branch", "archive", "removed"} {
		t.Run(kind, func(t *testing.T) {
			m, p := cleanupFixture(t)
			ctx := context.Background()
			main, e := git(ctx, m.Repository, "symbolic-ref", "HEAD")
			if e != nil {
				t.Fatal(e)
			}
			if kind == "branch" {
				if _, e = git(ctx, m.Repository, "worktree", "remove", p.Binding.Path); e != nil {
					t.Fatal(e)
				}
				git(ctx, m.Repository, "update-ref", "-d", "refs/heads/"+p.Binding.Branch)
				if _, e = git(ctx, m.Repository, "symbolic-ref", "refs/heads/"+p.Binding.Branch, main); e != nil {
					t.Fatal(e)
				}
				if _, e = m.PlanCleanup(ctx, p.Run, p.Task); e == nil {
					t.Fatal("symbolic branch accepted")
				}
			} else {
				// Same SHA prevents the older identity mismatch check from masking aliases.
				if _, e = git(ctx, m.Repository, "update-ref", main, p.Head); e != nil {
					t.Fatal(e)
				}
				if _, e = git(ctx, m.Repository, "symbolic-ref", p.Archive, main); e != nil {
					t.Fatal(e)
				}
				if kind == "removed" {
					if _, e = git(ctx, m.Repository, "worktree", "remove", p.Binding.Path); e != nil {
						t.Fatal(e)
					}
					if _, e = git(ctx, m.Repository, "update-ref", "-d", "refs/heads/"+p.Binding.Branch); e != nil {
						t.Fatal(e)
					}
					if _, e = m.PlanCleanup(ctx, p.Run, p.Task); e == nil {
						t.Fatal("symbolic completed archive accepted")
					}
				} else {
					if e = m.ArchiveCleanup(ctx, p, func(context.Context) error { return nil }); e == nil {
						t.Fatal("symbolic archive prepass accepted")
					}
					if e = m.Cleanup(ctx, p, func(context.Context) error { return nil }); e == nil {
						t.Fatal("symbolic archive accepted")
					}
					if _, e = os.Stat(p.Binding.Path); e != nil {
						t.Fatal("worktree deleted", e)
					}
					if _, e = git(ctx, m.Repository, "rev-parse", "refs/heads/"+p.Binding.Branch); e != nil {
						t.Fatal("branch deleted", e)
					}
				}
			}
			if _, e = git(ctx, m.Repository, "rev-parse", main); e != nil {
				t.Fatal("foreign branch affected", e)
			}
		})
	}
}

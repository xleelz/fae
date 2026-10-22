package worktree

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func manager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, args := range [][]string{{"init"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.invalid"}, {"commit", "--allow-empty", "-m", "base"}} {
		if _, err := git(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	return &Manager{Repository: repo, Root: filepath.Join(root, "trees")}
}
func TestIsolationAdoptionAndRemoval(t *testing.T) {
	m := manager(t)
	ctx := context.Background()
	a, err := m.Ensure(ctx, "r", "a", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Ensure(ctx, "r", "b", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Path, "own"), []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.Path, "own")); !os.IsNotExist(err) {
		t.Fatal("leaked")
	}
	again, err := m.Ensure(ctx, "r", "a", "HEAD")
	if err != nil || again != a {
		t.Fatalf("adoption: %v", err)
	}
	if m.Remove(ctx, "r", "a") == nil {
		t.Fatal("removed dirty worktree")
	}
	if err := m.Remove(ctx, "r", "b"); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentCreation(t *testing.T) {
	m := manager(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Ensure(context.Background(), "r", "t", "HEAD"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
func TestForeignAndEscapeRefused(t *testing.T) {
	m := manager(t)
	b := m.Binding("r", "t")
	if err := os.MkdirAll(m.Root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(m.Repository, b.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), "r", "t", "HEAD"); err == nil {
		t.Fatal("escape accepted")
	}
	if m.Remove(context.Background(), "r", "t") == nil {
		t.Fatal("foreign removed")
	}
}
func TestIdentityUnambiguous(t *testing.T) {
	if Identity("ab", "c") == Identity("a", "bc") {
		t.Fatal("ambiguous encoding")
	}
}

func TestIndependentManagersSerialize(t *testing.T) {
	m := manager(t)
	other := &Manager{Repository: m.Repository, Root: m.Root}
	var wg sync.WaitGroup
	for _, v := range []*Manager{m, other} {
		wg.Add(1)
		go func(v *Manager) {
			defer wg.Done()
			if _, err := v.Ensure(context.Background(), "shared", "task", "HEAD"); err != nil {
				t.Error(err)
			}
		}(v)
	}
	wg.Wait()
}
func TestRootSymlinkRefused(t *testing.T) {
	m := manager(t)
	target := t.TempDir()
	if err := os.Symlink(target, m.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), "r", "t", "HEAD"); err == nil {
		t.Fatal("symlink root accepted")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("wrote outside root")
	}
}

func TestHostileGitEnvironment(t *testing.T) {
	m := manager(t)
	foreign := manager(t)
	t.Setenv("GIT_DIR", filepath.Join(foreign.Repository, ".git"))
	t.Setenv("GIT_WORK_TREE", foreign.Repository)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(foreign.Repository, "injected-index"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.worktree")
	t.Setenv("GIT_CONFIG_VALUE_0", foreign.Repository)
	b, err := m.Ensure(context.Background(), "r", "safe", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.verify(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(foreign.Repository, "injected-index")); !os.IsNotExist(err) {
		t.Fatal("wrote foreign index")
	}
	if err = m.Remove(context.Background(), "r", "safe"); err != nil {
		t.Fatal(err)
	}
}

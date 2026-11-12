package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

// FAE-VCS-001: a resumed task whose branch survives without its directory
// adopts the branch and its commits; it never re-creates the branch.
func TestBranchWithoutDirectoryIsAdopted(t *testing.T) {
	m := manager(t)
	ctx := context.Background()
	a, err := m.Ensure(ctx, "r", "a", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(a.Path, "own"), []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "own"}, {"commit", "-m", "own work"}} {
		if _, err = git(ctx, a.Path, args...); err != nil {
			t.Fatal(err)
		}
	}
	head, _ := git(ctx, a.Path, "rev-parse", "HEAD")
	for _, prune := range []bool{false, true} {
		if err = os.RemoveAll(a.Path); err != nil {
			t.Fatal(err)
		}
		if prune {
			if _, err = git(ctx, m.Repository, "worktree", "prune"); err != nil {
				t.Fatal(err)
			}
		}
		again, err := m.Ensure(ctx, "r", "a", "HEAD")
		if err != nil || again != a {
			t.Fatalf("prune=%v: branch without directory not adopted: %v", prune, err)
		}
		if got, _ := git(ctx, a.Path, "rev-parse", "HEAD"); got != head {
			t.Fatal("adoption lost the task's commits", got, head)
		}
		if _, err = os.Stat(filepath.Join(a.Path, "own")); err != nil {
			t.Fatal("adopted worktree lacks committed work", err)
		}
	}
	// A branch checked out by any other worktree is never adopted twice.
	if err = os.RemoveAll(a.Path); err != nil {
		t.Fatal(err)
	}
	if _, err = git(ctx, m.Repository, "worktree", "prune"); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(m.Root), "other")
	if _, err = git(ctx, m.Repository, "worktree", "add", other, a.Branch); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Ensure(ctx, "r", "a", "HEAD"); err == nil {
		t.Fatal("branch checked out elsewhere adopted")
	}
}

// Refork replaces only a pristine task worktree's base and resumes an
// interrupted move; work or a moved branch is refused.
func TestReforkPristineOnly(t *testing.T) {
	m := manager(t)
	ctx := context.Background()
	base, _ := git(ctx, m.Repository, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(m.Repository, "landed"), []byte("prerequisite\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "landed"}, {"commit", "-m", "landed prerequisite"}} {
		if _, err := git(ctx, m.Repository, args...); err != nil {
			t.Fatal(err)
		}
	}
	tip, _ := git(ctx, m.Repository, "rev-parse", "HEAD")
	ensure := func(task string) Binding {
		b, err := m.Ensure(ctx, "r", task, base)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	at := func(b Binding, want string) {
		t.Helper()
		if got, _ := git(ctx, b.Path, "rev-parse", "HEAD"); got != want {
			t.Fatal("HEAD", got, "want", want)
		}
		if status, _ := git(ctx, b.Path, "status", "--porcelain"); status != "" {
			t.Fatal("not pristine", status)
		}
	}
	clean := ensure("clean")
	for i := 0; i < 2; i++ {
		if _, err := m.Refork(ctx, "r", "clean", base, tip); err != nil {
			t.Fatal(err)
		}
		at(clean, tip)
	}
	if _, err := os.Stat(filepath.Join(clean.Path, "landed")); err != nil {
		t.Fatal("reforked worktree lacks the tip's content")
	}
	interrupted := ensure("interrupted")
	if _, err := git(ctx, interrupted.Path, "read-tree", "-m", "-u", base, tip); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Refork(ctx, "r", "interrupted", base, tip); err != nil {
		t.Fatal("interrupted refork not resumed", err)
	}
	at(interrupted, tip)
	dirty := ensure("dirty")
	if err := os.WriteFile(filepath.Join(dirty.Path, "residue"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Refork(ctx, "r", "dirty", base, tip); err == nil {
		t.Fatal("dirty worktree reforked")
	}
	moved := ensure("moved")
	if _, err := git(ctx, moved.Path, "commit", "--allow-empty", "-m", "residue"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Refork(ctx, "r", "moved", base, tip); err == nil {
		t.Fatal("moved branch reforked")
	}
}

// A refork's tree move holds no manager lock: another task's worktree is
// created while a slowed move is in progress, and the ref CAS refuses a
// worktree that changed during the move (FAE-ROB-021).
func TestReforkTreeMoveHoldsNoManagerLock(t *testing.T) {
	m := manager(t)
	ctx := context.Background()
	base, _ := git(ctx, m.Repository, "rev-parse", "HEAD")
	if _, err := git(ctx, m.Repository, "commit", "--allow-empty", "-m", "landed prerequisite"); err != nil {
		t.Fatal(err)
	}
	tip, _ := git(ctx, m.Repository, "rev-parse", "HEAD")
	for _, task := range []string{"slow", "touched"} {
		if _, err := m.Ensure(ctx, "r", task, base); err != nil {
			t.Fatal(err)
		}
	}
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n*' read-tree -m -u '*) : > '%s'; sleep 3;;\nesac\nexec '%s' \"$@\"\n", started, real)
	if err = os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	refork := func(task string) chan error {
		done := make(chan error, 1)
		go func() { _, e := m.Refork(ctx, "r", task, base, tip); done <- e }()
		for !exists(started) {
			time.Sleep(5 * time.Millisecond)
		}
		return done
	}
	done := refork("slow")
	begun := time.Now()
	if _, err = m.Ensure(ctx, "r", "other", base); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begun); elapsed >= time.Second {
		t.Fatal("worktree creation waited for another task's refork", elapsed)
	}
	select {
	case err = <-done:
		t.Fatal("refork finished before the creation it should not block", err)
	default:
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if head, _ := git(ctx, m.Binding("r", "slow").Path, "rev-parse", "HEAD"); head != tip {
		t.Fatal("refork did not complete", head)
	}
	if err = os.Remove(started); err != nil {
		t.Fatal(err)
	}
	done = refork("touched")
	touched := m.Binding("r", "touched")
	if err = os.WriteFile(filepath.Join(touched.Path, "residue"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("worktree changed during the move was reforked")
	}
	if head, _ := git(ctx, touched.Path, "rev-parse", "HEAD"); head != base {
		t.Fatal("changed worktree's branch moved", head)
	}
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

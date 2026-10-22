package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAncestorSymlinkRefusedBeforeDirectoryCreation(t *testing.T) {
	m := manager(t)
	foreign := t.TempDir()
	parent, err := filepath.EvalSymlinks(filepath.Dir(m.Root))
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(foreign, link); err != nil {
		t.Fatal(err)
	}
	m.Root = filepath.Join(link, "must-not-create")
	if _, err := m.Ensure(context.Background(), "run", "task", "HEAD"); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	entries, err := os.ReadDir(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused request nevertheless wrote outside owned root: %v", entries)
	}
}

func TestContendedManagerLockHonorsCancellation(t *testing.T) {
	m := manager(t)
	if err := os.MkdirAll(m.Root, 0700); err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(m.Root)
	if err != nil {
		t.Fatal(err)
	}
	m.Root = physical
	unlock, err := m.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	other := &Manager{Repository: m.Repository, Root: m.Root}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := other.Ensure(ctx, "run", "task", "HEAD"); result <- err }()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected deadline, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lock did not honor cancellation")
	}
}

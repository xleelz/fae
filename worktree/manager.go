// Package worktree manages only deterministically named Fae task worktrees.
package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/xleelz/fae/workspace"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Manager struct {
	Repository, Root string
}
type Binding struct{ Path, Branch string }

func Identity(run, task string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s", len(run), run, len(task), task)))
	return hex.EncodeToString(sum[:])
}
func (m *Manager) Binding(run, task string) Binding {
	id := Identity(run, task)
	return Binding{filepath.Join(m.Root, id), "fae/task/" + id}
}
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// Ensure adopts only a registered worktree with the expected repository and branch.
func (m *Manager) Ensure(ctx context.Context, run, task, base string) (Binding, error) {
	b := m.Binding(run, task)
	if run == "" || task == "" {
		return b, fmt.Errorf("run and task required")
	}
	if err := workspace.Validate(m.Repository); err != nil {
		return b, err
	}
	if err := workspace.Validate(m.Root); err != nil {
		return b, err
	}
	if base == "" || strings.HasPrefix(base, "-") {
		return b, fmt.Errorf("invalid base")
	}
	if err := physicalAncestor(m.Root); err != nil {
		return b, err
	}
	if err := os.MkdirAll(m.Root, 0700); err != nil {
		return b, err
	}
	physical, err := filepath.EvalSymlinks(m.Root)
	if err != nil {
		return b, err
	}
	if physical != m.Root {
		return b, fmt.Errorf("worktree root must be a physical canonical path")
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return b, err
	}
	defer unlock()
	if _, err := os.Lstat(b.Path); err == nil {
		return b, m.verify(ctx, b)
	} else if !os.IsNotExist(err) {
		return b, err
	}
	if _, err := git(ctx, m.Repository, "rev-parse", "--verify", base+"^{commit}"); err != nil {
		return b, err
	}
	if _, err := git(ctx, m.Repository, "worktree", "add", "-b", b.Branch, b.Path, base); err != nil {
		return b, err
	}
	return b, m.verify(ctx, b)
}
func (m *Manager) verify(ctx context.Context, b Binding) error {
	if err := workspace.Contained(m.Root, b.Path); err != nil {
		return err
	}
	branch, err := git(ctx, b.Path, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return err
	}
	if branch != b.Branch {
		return fmt.Errorf("unexpected worktree branch")
	}
	a, err := git(ctx, m.Repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	c, err := git(ctx, b.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if a != c {
		return fmt.Errorf("foreign repository")
	}
	list, err := git(ctx, m.Repository, "worktree", "list", "--porcelain")
	if err != nil {
		return err
	}
	if !strings.Contains("\n"+list+"\n", "\nworktree "+b.Path+"\n") {
		return fmt.Errorf("unregistered worktree")
	}
	return nil
}

// Remove refuses dirty worktrees; it never uses force or recursively removes paths.
func (m *Manager) Remove(ctx context.Context, run, task string) error {
	b := m.Binding(run, task)
	unlock, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := m.verify(ctx, b); err != nil {
		return err
	}
	status, err := git(ctx, b.Path, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("dirty worktree: removal refused")
	}
	_, err = git(ctx, m.Repository, "worktree", "remove", b.Path)
	return err
}

// lock serializes independent managers and processes sharing this owned root.
func (m *Manager) lock(ctx context.Context) (func(), error) {
	if err := workspace.Validate(m.Root); err != nil {
		return nil, err
	}
	physical, err := filepath.EvalSymlinks(m.Root)
	if err != nil {
		return nil, err
	}
	if physical != m.Root {
		return nil, fmt.Errorf("worktree root must be a physical canonical path")
	}

	f, err := os.OpenFile(filepath.Join(m.Root, ".manager.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err = ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

// Inspect the existing ancestor before MkdirAll can create anything through it.
func physicalAncestor(path string) error {
	for {
		_, err := os.Lstat(path)
		if err == nil {
			physical, err := filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}
			if physical != path {
				return fmt.Errorf("symlink ancestor refused")
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return err
		}
		path = parent
	}
}

// Verify requires a registered task worktree belonging to this manager.
func (m *Manager) Verify(ctx context.Context, b Binding) error {
	if filepath.Dir(b.Path) != m.Root || !strings.HasPrefix(b.Branch, "fae/task/") || filepath.Base(b.Path) != strings.TrimPrefix(b.Branch, "fae/task/") {
		return fmt.Errorf("unowned task worktree")
	}
	return m.verify(ctx, b)
}

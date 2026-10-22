package effects

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/xleelz/fae/worktree"
)

// Reconciliation leaves the combined tree in working files and the new base in
// the index. Subsequent implementation edits therefore remain unstaged, including
// conflict resolution. Hold Git's real index lock across checkout and installation.
func (b *Broker) applyReconcileIndex(ctx context.Context, w worktree.Binding, r ReconcileEvidence) error {
	index, err := b.git(ctx, w, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return err
	}
	physical, err := filepath.EvalSymlinks(filepath.Dir(index))
	if err != nil || physical != filepath.Dir(index) || !filepath.IsAbs(index) || filepath.Clean(index) != index || filepath.Base(index) != "index" {
		return errors.New("physical Git index path required")
	}
	lockPath := index + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("lock reconciliation index: %w", err)
	}
	ownsLock := true
	defer func() {
		lock.Close()
		if ownsLock {
			os.Remove(lockPath)
		}
	}()
	current, err := os.OpenFile(index, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	info, err := current.Stat()
	if err != nil || !info.Mode().IsRegular() {
		current.Close()
		return errors.New("regular Git index required")
	}
	_, copyErr := io.Copy(lock, current)
	err = errors.Join(copyErr, current.Close(), lock.Close())
	if err != nil {
		return err
	}
	env := []string{"GIT_INDEX_FILE=" + lockPath}
	staged, err := b.git(ctx, w, env, "write-tree")
	if err != nil {
		return err
	}
	originalBase, err := b.git(ctx, w, nil, "rev-parse", r.OriginalBase+"^{tree}")
	if err != nil {
		return err
	}
	base, err := b.git(ctx, w, nil, "rev-parse", r.Base+"^{tree}")
	if err != nil {
		return err
	}
	head, err := b.git(ctx, w, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if !CanReconcileCommitIndex(staged == originalBase || staged == base || staged == r.OriginalTree || staged == r.Tree, head == r.OriginalBase || head == r.Base) {
		return errors.New("foreign staged index or changed reconcile head; index preserved")
	}
	if head != r.Base {
		if _, err = b.git(ctx, w, nil, "update-ref", "refs/heads/"+w.Branch, r.Base, head); err != nil {
			return err
		}
	}
	if _, err = b.git(ctx, w, env, "-c", "core.hooksPath=/dev/null", "read-tree", "--reset", "-u", r.Tree); err != nil {
		return fmt.Errorf("reconcile checkout incomplete; retry persisted plan: %w", err)
	}
	// No -u here: preserve the combined working files, unstage them against base.
	if _, err = b.git(ctx, w, env, "read-tree", r.Base); err != nil {
		return err
	}
	head, err = b.git(ctx, w, nil, "rev-parse", "HEAD")
	if err != nil || head != r.Base {
		return errors.New("reconcile head changed before index installation")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	prepared, err := os.OpenFile(lockPath, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	err = errors.Join(prepared.Sync(), prepared.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(lockPath, index); err != nil {
		return err
	}
	ownsLock = false
	directory, err := os.Open(filepath.Dir(index))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

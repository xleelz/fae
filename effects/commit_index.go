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

// ReconcileCommitIndex completes the index half of a journaled commit after an
// interrupted ref CAS. It changes neither worktree bytes nor the branch ref.
func (b *Broker) ReconcileCommitIndex(ctx context.Context, w worktree.Binding, p PreparedCommit) error {
	if err := b.validate(ctx, w); err != nil {
		return err
	}
	tree, err := b.Tree(ctx, w)
	if err != nil || tree != p.Snapshot.Tree {
		return errors.New("committed worktree differs from reviewed tree")
	}
	committedTree, err := b.git(ctx, w, nil, "rev-parse", p.SHA+"^{tree}")
	if err != nil || committedTree != p.Snapshot.Tree {
		return errors.New("committed tree differs from prepared object")
	}
	parent, err := b.git(ctx, w, nil, "rev-parse", p.SHA+"^")
	if err != nil || parent != p.Snapshot.Base {
		return errors.New("committed parent differs from prepared object")
	}
	return b.applyCommitIndex(ctx, w, p, false)
}

// Git's actual index lock spans the trusted-index check, optional ref CAS and
// atomic index installation. Private-index reads throughout planning stay pure.
func (b *Broker) applyCommitIndex(ctx context.Context, w worktree.Binding, p PreparedCommit, advance bool) error {
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
		return fmt.Errorf("lock Git index: %w", err)
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
	closeErr := current.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err = lock.Close(); err != nil {
		return err
	}
	env := []string{"GIT_INDEX_FILE=" + lockPath}
	staged, err := b.git(ctx, w, env, "write-tree")
	if err != nil {
		return err
	}
	baseTree, err := b.git(ctx, w, nil, "rev-parse", p.Snapshot.Base+"^{tree}")
	if err != nil {
		return err
	}
	expected := p.SHA
	if advance {
		expected = p.Snapshot.Base
	}
	head, err := b.git(ctx, w, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if !CanReconcileCommitIndex(staged == baseTree || staged == p.Snapshot.Tree, head == expected) {
		return errors.New("foreign staged index or changed commit head; index preserved")
	}
	// Prepare the exact new index before changing HEAD. No checkout/-u is used.
	if _, err = b.git(ctx, w, env, "read-tree", p.Snapshot.Tree); err != nil {
		return err
	}
	if advance {
		if _, err = b.git(ctx, w, nil, "update-ref", "refs/heads/"+w.Branch, p.SHA, p.Snapshot.Base); err != nil {
			return err
		}
	}
	head, err = b.git(ctx, w, nil, "rev-parse", "HEAD")
	if err != nil || head != p.SHA {
		return errors.New("commit head changed before index installation")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	prepared, err := os.OpenFile(lockPath, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	err = prepared.Sync()
	closeErr = prepared.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(lockPath, index); err != nil {
		return err
	}
	ownsLock = false
	directory, err := os.Open(filepath.Dir(index))
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr = directory.Close()
	if err != nil {
		return err
	}
	return closeErr
}

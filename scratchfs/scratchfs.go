// Package scratchfs removes the on-disk scratch directories Fae gives host
// commands: a brokered run's TMPDIR and policy directory, a landing's gate
// TMPDIR, a gate run's TMPDIR and a scratch instance being removed
// (FAE-HOST-020). A test can leave a tree there that os.RemoveAll cannot
// remove: Go's module cache is read-only (mode 0555 directories), so a test
// that points GOPATH or GOMODCACHE into its TMPDIR leaves one, and so does a
// run that unseals and fills its GOROOT mirror. RemoveAll makes such
// directories writable first, without following a symlink out of the tree.
package scratchfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// RemoveAll removes path and everything under it, as os.RemoveAll does, and
// also when directories in it are read-only or unreadable: after a failed
// first attempt it makes every directory of the tree owner-writable (0700)
// and tries again. Only path and directories inside it are changed, and a
// symlink is never followed: path itself is opened without following a link
// and changed through that handle, and the walk runs through an os.Root
// opened on that same handle (which refuses a link out of the tree) and
// reads entry types without following links. So no step resolves path again
// after it is checked, and a link swapped in for path, or for a directory in
// it, changes nothing outside (on Linux and macOS; elsewhere path is resolved
// by name). A path that does not exist is removed already. The error is the
// last removal's, joined with what stopped a directory being made writable.
func RemoveAll(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	info, statErr := os.Lstat(path)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return nil
	case statErr != nil || !info.IsDir():
		return err
	}
	writable := makeTreeWritable(path)
	if err = os.RemoveAll(path); err == nil {
		return nil
	}
	return errors.Join(err, writable)
}

// makeTreeWritable gives dir and every directory under it mode 0700, parents
// before children, so each can then be listed and emptied. dir itself is
// opened as a directory, never through a link, and every later step works
// through that handle (openTopDir).
func makeTreeWritable(dir string) error {
	root, err := openTopDir(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	var errs []error
	// WalkDir calls the function for a directory before it reads it, so a
	// directory without read or search permission is opened after its mode
	// changes. Entry types come from the parent's listing (lstat), so a
	// symlink to a directory is neither changed nor entered.
	walked := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		if d.IsDir() && name != "." {
			if e := root.Chmod(name, 0700); e != nil {
				errs = append(errs, e)
			}
		}
		return nil
	})
	return errors.Join(walked, errors.Join(errs...))
}

// topOpened, when set, runs once makeTreeWritable has checked its top
// directory and before that directory's mode changes; tests swap the path
// there.
var topOpened func(dir string)

// Quarantine moves path, a directory RemoveAll could not remove, out of the
// way into a new private directory under dir and returns its new path, so a
// sweep that found it does not find it again and the operator can inspect or
// remove it. dir must be on path's file system (a rename); it is created
// private when missing.
func Quarantine(path, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	holder, err := os.MkdirTemp(dir, filepath.Base(path)+"-")
	if err != nil {
		return "", err
	}
	moved := filepath.Join(holder, filepath.Base(path))
	if err = os.Rename(path, moved); err != nil {
		_ = os.Remove(holder)
		return "", err
	}
	return moved, nil
}

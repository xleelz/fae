package scratchfs

import (
	"os"
	"path/filepath"
	"testing"
)

// readOnlyTree plants, under dir/tree, what a test leaves in its TMPDIR when
// it points GOMODCACHE there (Go's read-only module cache, mode 0555
// directories and 0444 files), a directory nobody may list (0000) and one
// only listable (0500), plus links out of the tree to a read-only directory
// and a file outside it. It returns the tree and the outside directory.
func readOnlyTree(t *testing.T, dir string) (tree, outside string) {
	t.Helper()
	tree, outside = filepath.Join(dir, "tree"), filepath.Join(dir, "outside")
	module := filepath.Join(tree, "gopath", "pkg", "mod", "example.com", "m@v1.0.0")
	for _, d := range []string{filepath.Join(module, "sub"), filepath.Join(tree, "locked", "inner"), filepath.Join(tree, "listed"), outside} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(module, "go.mod"), filepath.Join(module, "sub", "x.go"), filepath.Join(tree, "locked", "inner", "f"), filepath.Join(tree, "listed", "f"), filepath.Join(outside, "keep")} {
		if err := os.WriteFile(f, []byte("x\n"), 0444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(module, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "keep"), filepath.Join(tree, "listed", "file-link")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(module, "sub"), module, filepath.Join(tree, "gopath", "pkg", "mod", "example.com"), filepath.Join(tree, "gopath", "pkg", "mod")} {
		if err := os.Chmod(d, 0555); err != nil {
			t.Fatal(err)
		}
	}
	for d, mode := range map[string]os.FileMode{filepath.Join(tree, "locked", "inner"): 0000, filepath.Join(tree, "listed"): 0500, outside: 0555} {
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0700) })
	return tree, outside
}

// FAE-HOST-020: a scratch tree holding read-only and unreadable directories
// is removed, which os.RemoveAll cannot do, and nothing outside it changes
// through a link.
func TestRemoveAllRemovesReadOnlyTreesWithoutFollowingLinks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	tree, outside := readOnlyTree(t, t.TempDir())
	if err := os.RemoveAll(tree); err == nil {
		t.Fatal("os.RemoveAll removed a read-only tree; the test no longer shows the failure")
	}
	if err := RemoveAll(tree); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatal("tree left on disk", err)
	}
	info, err := os.Stat(outside)
	if err != nil || info.Mode().Perm() != 0555 {
		t.Fatal("a linked directory outside the tree changed", info, err)
	}
	if body, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(body) != "x\n" {
		t.Fatal("a linked file outside the tree changed", err)
	}
	if err = RemoveAll(tree); err != nil {
		t.Fatal("removing a missing tree", err)
	}
	link := filepath.Join(filepath.Dir(tree), "link")
	if err = os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err = RemoveAll(link); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("link left", err)
	}
	if info, err = os.Stat(outside); err != nil || info.Mode().Perm() != 0555 {
		t.Fatal("removing a link changed its target", info, err)
	}
}

// Quarantine moves a tree into a new private directory under the quarantine
// root, out of a sweep's way.
func TestQuarantineMovesATreeAside(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "command-tmp-1")
	if err := os.MkdirAll(filepath.Join(tree, "x"), 0700); err != nil {
		t.Fatal(err)
	}
	moved, err := Quarantine(tree, filepath.Join(dir, "quarantine"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(moved) != "command-tmp-1" || filepath.Dir(filepath.Dir(moved)) != filepath.Join(dir, "quarantine") {
		t.Fatal("moved to", moved)
	}
	if _, err = os.Stat(filepath.Join(moved, "x")); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatal("tree still in place", err)
	}
	info, err := os.Stat(filepath.Join(dir, "quarantine"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("quarantine root not private", info, err)
	}
	if _, err = Quarantine(filepath.Join(dir, "missing"), filepath.Join(dir, "quarantine")); err == nil {
		t.Fatal("quarantined a missing tree")
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "quarantine")); len(entries) != 1 {
		t.Fatal("a failed quarantine left a holder", entries)
	}
}

// FAE-HOST-020: a link swapped in for the tree's own path after RemoveAll
// checked it changes nothing outside. Before, makeTreeWritable checked the
// path with Lstat and then chmodded and opened it by name again, so a link
// to an outside directory put there in between had that directory and its
// subdirectories made 0700 (12 races in 2,000 in re-audit 10's probe). Now
// the top directory is opened once, refusing a link, and changed and walked
// through that handle; the swap is made deterministic at the point between
// the check and the first change (topOpened).
func TestRemoveAllUsesTheDirectoryItCheckedWhenTheTopIsSwapped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	tree, outside := readOnlyTree(t, dir)
	sub := filepath.Join(outside, "sub")
	if err := os.Chmod(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sub, 0555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0700) })
	moved := tree + ".moved"
	swapped := false
	topOpened = func(path string) {
		if path != tree || swapped {
			return
		}
		swapped = true
		if err := os.Rename(tree, moved); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(outside, tree); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { topOpened = nil })
	_ = RemoveAll(tree)
	if !swapped {
		t.Fatal("the swap never ran")
	}
	for _, d := range []string{outside, sub} {
		if info, err := os.Lstat(d); err != nil || info.Mode().Perm() != 0555 {
			t.Fatalf("%s outside the tree changed through the swapped link: %v %v", d, info.Mode(), err)
		}
	}
	if body, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(body) != "x\n" {
		t.Fatal("a file outside the tree changed", err)
	}
	// The directory it checked was made writable, under its new name.
	if info, err := os.Stat(filepath.Join(moved, "locked", "inner")); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("the checked tree was not made writable through its handle", info, err)
	}
	if err := RemoveAll(moved); err != nil {
		t.Fatal(err)
	}
}

// FAE-HOST-020: a top directory nobody may list (mode 0000) is still made
// writable and removed: it is opened without needing permission on it.
func TestRemoveAllRemovesAnUnlistableTopDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	tree := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(tree, "x"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tree, 0000); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(tree); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatal("unlistable top directory left", err)
	}
}

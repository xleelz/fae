// Package workspace enforces byte-preserving paths at filesystem boundaries.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Validate rejects non-canonical input rather than silently cleaning it.
func Validate(path string) error {
	if !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 || !strings.HasPrefix(path, "/") {
		return fmt.Errorf("invalid absolute path %q", path)
	}
	if path == "/" {
		return nil
	}
	for _, part := range strings.Split(path[1:], "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("non-canonical path %q", path)
		}
	}
	return nil
}

// Contained checks existing paths, including symlinks, at time of use.
// Callers must serialize filesystem mutation; this is not a sandbox against hostile races.
func Contained(root, path string) error {
	if err := Validate(root); err != nil {
		return err
	}
	if err := Validate(path); err != nil {
		return err
	}
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(r, p)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("path %q escapes root %q", path, root)
	}
	return nil
}

// CheckBinding performs all refusal checks without writing anything.
func CheckBinding(declared, current, stored string) error {
	for _, p := range []string{declared, current} {
		if err := Validate(p); err != nil {
			return err
		}
	}
	if declared != current {
		return fmt.Errorf("current directory differs from declared workspace")
	}
	if stored != "" && stored != declared {
		return fmt.Errorf("instance is bound to a different workspace")
	}
	actual, err := filepath.EvalSymlinks(declared)
	if err != nil {
		return err
	}
	if actual != declared {
		return fmt.Errorf("workspace must use its canonical physical path")
	}
	info, err := os.Stat(declared)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace is not a directory")
	}
	return nil
}

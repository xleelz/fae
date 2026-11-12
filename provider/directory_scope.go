package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/xleelz/fae/workflow"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

type directoryFile struct {
	digest string
	data   []byte
	mode   os.FileMode
}
type DirectorySnapshot map[string]directoryFile

func SnapshotTaskDirectory(root string) (DirectorySnapshot, error) {
	snapshot := DirectorySnapshot{}
	if root == "" {
		return snapshot, nil
	}
	if err := PhysicalTaskDirectory(root); err != nil {
		return nil, err
	}
	err := filepath.WalkDir(root, func(full string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		st, err := os.Lstat(full)
		if err != nil {
			return err
		}
		var data []byte
		if st.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(full)
			if e != nil {
				return e
			}
			data = []byte(target)
		} else if st.Mode().IsRegular() {
			data, err = os.ReadFile(full)
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("unsupported file in task directory %s", rel)
		}
		snapshot[rel] = directoryFile{data: data, mode: st.Mode()}
		return nil
	})
	return snapshot, err
}

// VerifyReadOnlyTaskDirectory restores every changed path in a read-only
// directory, including newly created files, before refusing the turn.
func VerifyReadOnlyTaskDirectory(root string, before DirectorySnapshot) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	after, err := SnapshotTaskDirectory(root)
	if err != nil {
		return nil, err
	}
	changed := []string{}
	names := map[string]bool{}
	for name := range before {
		names[name] = true
	}
	for name := range after {
		names[name] = true
	}
	for name := range names {
		a, aok := before[name]
		b, bok := after[name]
		if aok == bok && a.mode == b.mode && bytes.Equal(a.data, b.data) {
			continue
		}
		changed = append(changed, name)
	}
	// Remove newly introduced leaves before recreating original symlinks.
	// This avoids traversing a replacement parent link during restoration.
	removal := append([]string{}, changed...)
	sort.Slice(removal, func(i, j int) bool { return len(removal[i]) > len(removal[j]) })
	for _, name := range removal {
		if _, existed := before[name]; !existed {
			target, err := safePath(root, name)
			if err != nil {
				if st, e := os.Lstat(filepath.Join(root, name)); e == nil && st.Mode()&os.ModeSymlink != 0 {
					target = filepath.Join(root, name)
				} else {
					return changed, err
				}
			}
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return changed, err
			}
		}
	}
	sort.Slice(changed, func(i, j int) bool { return len(changed[i]) < len(changed[j]) })
	for _, name := range changed {
		a, existed := before[name]
		if !existed {
			continue
		}
		target := filepath.Join(root, name)
		// The leaf itself may be a link we need to replace; parents may not.
		if err := EnsurePhysicalTaskDirectory(filepath.Dir(target)); err != nil {
			return changed, err
		}
		if st, err := os.Lstat(target); err == nil && st.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(target); err != nil {
				return changed, err
			}
		}
		if a.mode&os.ModeSymlink != 0 {
			if _, err := os.Lstat(target); err == nil {
				if err := os.Remove(target); err != nil {
					return changed, err
				}
			}
			if err := os.Symlink(string(a.data), target); err != nil {
				return changed, err
			}
		} else if err := os.WriteFile(target, a.data, a.mode.Perm()); err != nil {
			return changed, err
		}
	}

	sort.Strings(changed)
	return changed, nil
}

// CheckTaskDirectoryWrites keeps permitted outputs, restores forbidden ones,
// and returns the exact content digest of changed files that remain.
func CheckTaskDirectoryWrites(root string, before DirectorySnapshot, patterns []string, allowAll bool, scopeLayers ...[][]string) ([]string, string, error) {
	after, err := SnapshotTaskDirectory(root)
	if err != nil {
		return nil, "", err
	}
	restoreTo := DirectorySnapshot{}
	for name, file := range before {
		restoreTo[name] = file
	}
	names := map[string]bool{}
	for name := range before {
		names[name] = true
	}
	for name := range after {
		names[name] = true
	}
	changed := []string{}
	offending := []string{}
	for name := range names {
		a, aok := before[name]
		b, bok := after[name]
		if aok == bok && a.mode == b.mode && bytes.Equal(a.data, b.data) {
			continue
		}
		changed = append(changed, filepath.ToSlash(name))
		allowed := allowAll || workflow.MatchesAnyGlob(filepath.ToSlash(name), patterns)
		if len(scopeLayers) > 0 && len(scopeLayers[0]) > 0 {
			allowed = allowed && workflow.AllowsWriteScopes(filepath.ToSlash(name), scopeLayers[0])
		}
		if allowed {
			if bok {
				restoreTo[name] = b
			} else {
				delete(restoreTo, name)
			}
		} else {
			offending = append(offending, name)
		}
	}
	if len(offending) > 0 {
		if _, err := VerifyReadOnlyTaskDirectory(root, restoreTo); err != nil {
			return changed, "", err
		}
		sort.Strings(offending)
		return changed, "", fmt.Errorf("task directory writes outside scope: %v", offending)
	}
	sort.Strings(changed)
	h := sha256.New()
	for _, name := range changed {
		file, ok := after[filepath.FromSlash(name)]
		if !ok {
			fmt.Fprintf(h, "%s\\x00deleted\\x00", name)
			continue
		}
		fmt.Fprintf(h, "%s\\x00%d\\x00", name, file.mode)
		_, _ = h.Write(file.data)
	}
	return changed, hex.EncodeToString(h.Sum(nil)), nil
}

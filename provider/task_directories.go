package provider

import (
	"fmt"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"strings"
)

// taskDirectoryPath exposes only the explicitly granted task directories. It
// resolves every existing ancestor and refuses symlinks, including dangling
// links on a newly created path.
func (t *AgenticToolbox) taskDirectoryPath(name string, write bool) (string, bool, error) {
	target := name
	if t.ArtifactsOnly && write && !filepath.IsAbs(name) {
		target = filepath.Join(t.OutputDir, filepath.FromSlash(name))
	}
	for _, entry := range []struct {
		root     string
		writable bool
	}{{t.OutputDir, t.ArtifactsOnly}, {t.StagingDir, t.StagingWrite}} {
		if entry.root == "" {
			continue
		}
		root := filepath.Clean(entry.root)
		if !filepath.IsAbs(target) || target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			continue
		}
		if write {
			if allowed, e := OutputWriteAllowed(t.OutputOwnershipRegistry, t.TaskID, target); e != nil || !allowed {
				if e != nil {
					return "", true, e
				}
				return "", true, fmt.Errorf("descendant task output is read-only: %s", name)
			}
			for _, excluded := range t.ExcludedOutputDirs {
				if physicalWithin(excluded, filepath.Clean(target)) {
					return "", true, fmt.Errorf("descendant task output is read-only: %s", name)
				}
			}
		}
		if write && !entry.writable {
			return "", true, fmt.Errorf("task directory is read-only: %s", name)
		}
		if filepath.Clean(target) != target {
			return "", true, fmt.Errorf("unsafe task directory path %s", name)
		}
		rel, err := filepath.Rel(root, target)
		if err != nil {
			return "", true, err
		}
		if !write && rel != "." && t.Guard != nil && !t.Guard.AllowPath(filepath.ToSlash(rel)) {
			t.Guard.auditRefusal(name)
			return "", true, fmt.Errorf("task directory read refused by effective policy: %s", name)
		}
		if write && root == t.OutputDir && len(t.WriteScopes) > 0 && !workflow.AllowsWriteScopes(filepath.ToSlash(rel), t.WriteScopes) {
			return "", true, fmt.Errorf("artifact output outside effective write scope: %s", name)
		}
		current := root
		parts := append([]string{""}, strings.Split(rel, string(filepath.Separator))...)
		for _, part := range parts {
			if part != "" && part != "." {
				current = filepath.Join(current, part)
			}
			st, e := os.Lstat(current)
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return "", true, e
			}
			if st.Mode()&os.ModeSymlink != 0 {
				return "", true, fmt.Errorf("symlink in task directory path %s", name)
			}
		}
		return target, true, nil
	}
	return "", false, nil
}

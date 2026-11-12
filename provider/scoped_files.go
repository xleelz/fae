package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/xleelz/fae/workflow"
)

// ExpandScopedFiles includes new and ignored files while excluding scratch.
func ExpandScopedFiles(root string, patterns, scratch []string) ([]string, error) {
	set := map[string]bool{}
	for _, p := range patterns {
		if workflow.DeclaredPathSafe(p) && !strings.ContainsAny(p, "*?[") && !workflow.MatchesAnyGlob(p, scratch) {
			set[p] = true
		}
	}
	err := filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, full)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if workflow.MatchesAnyGlob(rel, patterns) && !workflow.MatchesAnyGlob(rel, scratch) {
			set[rel] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if isGitWorktree(root) {
		cmd := exec.Command("git", "-C", root, "ls-tree", "-r", "--name-only", "-z", "HEAD")
		cmd.Env = cleanGitEnv()
		raw, e := cmd.Output()
		if e != nil {
			return nil, e
		}
		for _, rel := range strings.Split(string(raw), "\x00") {
			if rel != "" && workflow.MatchesAnyGlob(rel, patterns) && !workflow.MatchesAnyGlob(rel, scratch) {
				set[rel] = true
			}
		}
	}
	files := make([]string, 0, len(set))
	for p := range set {
		files = append(files, p)
	}
	slices.Sort(files)
	return files, nil
}
func SnapshotScopedTree(ctx context.Context, root, base string, editable, scratch []string) (string, error) {
	var err error
	if base == "" {
		base, err = SnapshotWorktreeTree(ctx, root)
		if err != nil {
			return "", err
		}
	}
	base, err = excludeScratchTree(ctx, root, base, scratch)
	if err != nil {
		return "", err
	}
	files, err := ExpandScopedFiles(root, editable, scratch)
	if err != nil {
		return "", err
	}
	return SnapshotInScopeTree(ctx, root, base, files)
}
func VerifyAndRevertScopedWorktree(ctx context.Context, root, start string, editable, readonly []string, producer bool, layers [][]string, scratch []string, artifactsOnly bool) ([]string, error) {
	if start == "" {
		return nil, nil
	}
	allowed := func(rel string) bool {
		if artifactsOnly || workflow.MatchesAnyGlob(rel, readonly) {
			return false
		}
		if _, protected := workflow.ProtectedPath(rel); protected {
			return false
		}
		if workflow.MatchesAnyGlob(rel, scratch) {
			return true
		}
		if artifactsOnly || workflow.MatchesAnyGlob(rel, readonly) {
			return false
		}
		if len(layers) > 0 {
			return workflow.AllowsWriteScopes(rel, layers)
		}
		return producer && workflow.MatchesAnyGlob(rel, editable)
	}
	if isGitWorktree(root) {
		return gitVerifyAndRevert(ctx, root, start, allowed)
	}
	return fallbackVerifyAndRevert(root, start, allowed)
}
func (t *AgenticToolbox) scopedShellCWD() string {
	if t.ArtifactsOnly && t.OutputDir != "" {
		return t.OutputDir
	}
	return t.Worktree
}
func PhysicalTaskDirectory(root string) error {
	if root == "" {
		return nil
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("task directory must be canonical absolute")
	}
	for current := root; ; current = filepath.Dir(current) {
		st, e := os.Lstat(current)
		if e != nil {
			return e
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return fmt.Errorf("task directory is not a physical directory: %s", root)
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}

// EnsurePhysicalTaskDirectory creates one checked component at a time, never
// following an existing symbolic link during task directory allocation.
func EnsurePhysicalTaskDirectory(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("task directory must be canonical absolute")
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(root, current), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err = os.Mkdir(current, 0700); err != nil && !os.IsExist(err) {
				return err
			}
			st, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("non-physical task directory component %s", current)
		}
	}
	return nil
}

func excludeScratchTree(ctx context.Context, root, tree string, scratch []string) (string, error) {
	if len(scratch) == 0 {
		return tree, nil
	}
	if isGitWorktree(root) {
		dir, err := os.MkdirTemp(filepath.Dir(root), "fae-scratch-index-")
		if err != nil {
			return "", err
		}
		index := filepath.Join(dir, "index")
		defer func() { _ = os.Remove(index); _ = os.Remove(index + ".lock"); _ = os.Remove(dir) }()
		run := func(args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, systemGitBin(), append([]string{"-C", root}, args...)...)
			cmd.Env = cleanGitEnv("GIT_INDEX_FILE=" + index)
			raw, err := cmd.CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("scratch-free checkpoint: %w: %s", err, raw)
			}
			return strings.TrimSpace(string(raw)), nil
		}
		if _, err = run("read-tree", tree); err != nil {
			return "", err
		}
		names, err := run("ls-files", "-z")
		if err != nil {
			return "", err
		}
		for _, name := range strings.Split(names, "\x00") {
			if workflow.MatchesAnyGlob(name, scratch) {
				if _, err = run("update-index", "--force-remove", "--", name); err != nil {
					return "", err
				}
			}
		}
		return run("write-tree")
	}
	fallbackTreesMu.Lock()
	source, ok := fallbackTrees[tree]
	fallbackTreesMu.Unlock()
	if !ok {
		return "", fmt.Errorf("checkpoint tree missing")
	}
	filtered := fallbackTreeSnapshot{Files: map[string][]byte{}, Modes: map[string]os.FileMode{}}
	var names []string
	for name, data := range source.Files {
		if !workflow.MatchesAnyGlob(name, scratch) {
			filtered.Files[name] = data
			filtered.Modes[name] = source.Modes[name]
			names = append(names, name)
		}
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(filtered.Files[name]))
		_, _ = h.Write(filtered.Files[name])
	}
	id := hex.EncodeToString(h.Sum(nil))[:40]
	fallbackTreesMu.Lock()
	fallbackTrees[id] = filtered
	fallbackTreesMu.Unlock()
	return id, nil
}
func RestoreCheckpointPreservingScratch(ctx context.Context, root, tree string, scratch []string) error {
	if len(scratch) == 0 {
		return RestoreWorktreeCheckpoint(ctx, root, tree)
	}
	allowed := func(path string) bool { return workflow.MatchesAnyGlob(path, scratch) }
	if isGitWorktree(root) {
		_, err := gitVerifyAndRevert(ctx, root, tree, allowed)
		return err
	}
	_, err := fallbackVerifyAndRevert(root, tree, allowed)
	return err
}

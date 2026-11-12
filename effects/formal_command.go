package effects

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/xleelz/fae/proof"
)

// formalSourceDigest binds a receipt to source bytes rather than a tool label.
// Explicit sources are physical regular files, not globs, directories or links.
// The broker compares before/after digests even for declared scratch writes.
func formalSourceDigest(worktree, tree string, sources []string) (string, error) {
	if len(sources) == 0 {
		if strings.TrimSpace(tree) == "" {
			return "", fmt.Errorf("formal evidence requires a source tree identity")
		}
		return "git-tree:" + tree, nil
	}
	if len(sources) > 10000 {
		return "", fmt.Errorf("formal source list exceeds 10000 files")
	}
	physical, err := filepath.EvalSymlinks(worktree)
	if err != nil || physical != filepath.Clean(worktree) {
		return "", fmt.Errorf("formal sources require a physical worktree")
	}
	root, err := os.OpenRoot(physical)
	if err != nil {
		return "", err
	}
	defer root.Close()
	names := slices.Clone(sources)
	slices.Sort(names)
	hash := sha256.New()
	remaining := int64(64 << 20)
	for i, name := range names {
		if name == "" || !filepath.IsLocal(name) || filepath.Clean(name) != name || strings.ContainsAny(name, "\\*?[]\x00\r\n") {
			return "", fmt.Errorf("formal source must be a literal clean relative file: %q", name)
		}
		if i > 0 && name == names[i-1] {
			return "", fmt.Errorf("duplicate formal source: %s", name)
		}
		path := filepath.Join(physical, name)
		resolved, e := filepath.EvalSymlinks(path)
		if e != nil || resolved != path {
			return "", fmt.Errorf("formal source must be physical: %s", name)
		}
		info, e := os.Lstat(path)
		if e != nil {
			return "", e
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("formal source is not a regular file: %s", name)
		}
		if info.Size() > remaining {
			return "", fmt.Errorf("formal source bytes exceed 64 MiB")
		}
		file, e := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if e != nil {
			return "", e
		}
		opened, e := file.Stat()
		if e != nil {
			file.Close()
			return "", e
		}
		if !os.SameFile(info, opened) {
			file.Close()
			return "", fmt.Errorf("formal source changed while opening: %s", name)
		}
		fmt.Fprintf(hash, "%d:%s\x00%d\x00", len(name), name, info.Size())
		count, e := io.Copy(hash, io.LimitReader(file, remaining+1))
		closeErr := file.Close()
		if e != nil {
			return "", e
		}
		if closeErr != nil {
			return "", closeErr
		}
		if count > remaining {
			return "", fmt.Errorf("formal source bytes exceed 64 MiB")
		}
		if count != info.Size() {
			return "", fmt.Errorf("formal source changed while reading: %s", name)
		}
		remaining -= count
	}
	return "sha256:" + fmt.Sprintf("%x", hash.Sum(nil)), nil
}

// formalResult keeps a structured nonpassing receipt on parser errors; output,
// process status and cost remain real execution evidence in RunEvidence.
func formalResult(c Command, r *RunEvidence, sourceDigest string) {
	if c.Formal == nil {
		return
	}
	result, err := proof.Parse(*c.Formal, proof.Run{Stdout: r.Output, Stderr: r.Stderr, ExitCode: r.ExitCode, TimedOut: r.TimedOut, Truncated: r.Truncated, SourceDigest: sourceDigest})
	if err != nil {
		result.Outcome = proof.Error
		result.Passed = false
		result.Detail = fmt.Sprintf("formal report normalization failed: %v", err)
	}

	r.Proof = &result
}

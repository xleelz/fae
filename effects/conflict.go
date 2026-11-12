package effects

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/worktree"
)

func (b *Broker) rawGit(ctx context.Context, w worktree.Binding, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", w.Path}, args...)...)
	cmd.Env = b.literalPathGitEnv(w, nil)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out, nil
}

type ownHunk struct {
	label          string
	added, removed []string
}

func parseHunks(diff string) []ownHunk {
	var out []ownHunk
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "@@ ") {
			out = append(out, ownHunk{label: line})
			continue
		}
		if len(out) == 0 || line == "" {
			continue
		}
		h := &out[len(out)-1]
		if line[0] == '+' {
			h.added = append(h.added, line[1:])
		}
		if line[0] == '-' {
			h.removed = append(h.removed, line[1:])
		}
	}
	return out
}
func linesBlock(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return "\n" + strings.Join(lines, "\n") + "\n"
}

// VerifyConflictResolution validates against the immutable pre-conflict review,
// never a new agent report. Each original added block must remain in the new own
// delta, and each removed block must be absent from the resolved file. Offsets
// can move; named lost hunks fail closed. It intentionally refuses ambiguous
// deletion/move cases and partial-upstream shrinking deltas.
func (b *Broker) VerifyConflictResolution(ctx context.Context, w worktree.Binding, r ReconcileEvidence) error {
	if err := b.validate(ctx, w); err != nil {
		return err
	}
	head, err := b.git(ctx, w, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != r.Base {
		return fmt.Errorf("conflict resolution base changed")
	}
	resolved, err := b.Tree(ctx, w)
	if err != nil {
		return err
	}
	paths, err := b.changedPaths(ctx, w, r.OwnBase, r.OriginalTree)
	if err != nil {
		return err
	}
	current, err := b.changedPaths(ctx, w, r.Base, resolved)
	if err != nil {
		return err
	}
	changed := map[string]bool{}
	for _, p := range current {
		changed[p] = true
	}
	lost := &ReconcileError{Kind: "own-hunk-loss"}
	for _, path := range paths {
		// A file whose exact own version upstream already holds, and the
		// resolution keeps, is contained, not lost (FAE-VCS-007).
		upstream, err := b.pathsEqual(ctx, w, r.OriginalTree, r.Base, []string{path})
		if err != nil {
			return err
		}
		kept, err := b.pathsEqual(ctx, w, r.OriginalTree, resolved, []string{path})
		if err != nil {
			return err
		}
		if upstream && kept {
			continue
		}
		before, err := b.rawGit(ctx, w, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--unified=0", r.OwnBase, r.OriginalTree, "--", path)
		if err != nil {
			return err
		}
		after, err := b.rawGit(ctx, w, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--unified=0", r.Base, resolved, "--", path)
		if err != nil {
			return err
		}
		hunks := parseHunks(string(before))
		newHunks := parseHunks(string(after))
		var additions []string
		for _, h := range newHunks {
			additions = append(additions, h.added...)
		}
		available := linesBlock(additions)
		blob, blobErr := b.rawGit(ctx, w, "show", resolved+":"+path)
		if blobErr != nil {
			exists, e := b.git(ctx, w, nil, "ls-tree", resolved, "--", path)
			if e != nil || exists != "" {
				return fmt.Errorf("cannot inspect resolved path %s", path)
			}
			blob = nil
		}
		contents := "\n" + string(blob)
		if !strings.HasSuffix(contents, "\n") {
			contents += "\n"
		}
		for _, line := range strings.Split(string(blob), "\n") {
			if strings.HasPrefix(line, "<<<<<<< ") || line == "=======" || strings.HasPrefix(line, ">>>>>>> ") {
				return &ReconcileError{Kind: "unresolved-conflict", Paths: []string{path}}
			}
		}
		bad := !changed[path]
		// Preserve explicit task-owned mode changes as well as text hunks.
		mode := func(tree string) (string, error) {
			entry, e := b.git(ctx, w, nil, "ls-tree", tree, "--", path)
			if e != nil {
				return "", e
			}
			fields := strings.Fields(entry)
			if len(fields) == 0 {
				return "", nil
			}
			return fields[0], nil
		}
		originalMode, e := mode(r.OwnBase)
		if e != nil {
			return e
		}
		ownMode, e := mode(r.OriginalTree)
		if e != nil {
			return e
		}
		resolvedMode, e := mode(resolved)
		if e != nil {
			return e
		}
		if originalMode != ownMode && ownMode != resolvedMode {
			bad = true
			lost.Hunks = append(lost.Hunks, path+" mode change")
		}
		if len(hunks) == 0 {
			// Binary or mode-only own changes must remain exactly patch-identical.
			old, e := b.ownPatch(ctx, w, r.OwnBase, r.OriginalTree, path)
			if e != nil {
				return e
			}
			now, e := b.ownPatch(ctx, w, r.Base, resolved, path)
			if e != nil {
				return e
			}
			bad = bad || old != now
		}
		for _, h := range hunks {
			add, remove := linesBlock(h.added), linesBlock(h.removed)
			keepAdd := add == "" || strings.Contains(available, add)
			keepRemove := remove == "" || !strings.Contains(contents, remove)
			if !kernel.CanKeepOwnHunk(keepAdd, keepRemove) {
				bad = true
				lost.Hunks = append(lost.Hunks, path+" "+h.label)
			} else if add != "" {
				available = strings.Replace(available, add, "\n", 1)
			}
		}
		if bad {
			lost.Paths = append(lost.Paths, path)
			if len(hunks) == 0 {
				lost.Hunks = append(lost.Hunks, path+" mode/binary delta")
			}
		}
	}
	if len(lost.Paths) > 0 {
		return lost
	}
	return nil
}

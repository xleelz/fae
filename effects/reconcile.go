package effects

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"sort"
	"strings"

	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/worktree"
)

// ReconcileEvidence is a write-ahead plan. Persist it before ApplyReconcile.
// Tree is deliberately uncommitted: gates must run on it before Commit/Integrate.
type ReconcileEvidence struct {
	OriginalBase, OriginalTree, OwnBase, Base, Tree, Ref string
	Files, ChangedPaths                                  []string
	Changed, PatchIdentical, AlreadyContained            bool
}

// ReconcileError preserves machine-readable paths for implementation remediation.
type ReconcileError struct {
	Kind  string
	Paths []string
	Hunks []string `json:",omitempty"`
}

func (e *ReconcileError) Error() string {
	return fmt.Sprintf("reconcile %s: %s %s", e.Kind, strings.Join(e.Paths, ", "), strings.Join(e.Hunks, "; "))
}

func objectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (b *Broker) integrationTip(ctx context.Context, w worktree.Binding, ref string) (string, error) {
	if !strings.HasPrefix(ref, "refs/heads/fae/") || strings.HasPrefix(ref, "refs/heads/fae/task/") {
		return "", fmt.Errorf("integration ref must be an owned fae branch")
	}
	if _, err := b.git(ctx, w, nil, "check-ref-format", ref); err != nil {
		return "", err
	}
	list, err := b.git(ctx, w, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	if strings.Contains("\n"+list+"\n", "\nbranch "+ref+"\n") {
		return "", fmt.Errorf("integration ref is checked out")
	}
	return b.git(ctx, w, nil, "rev-parse", "--verify", ref+"^{commit}")
}
func (b *Broker) changedPaths(ctx context.Context, w worktree.Binding, base, tree string) ([]string, error) {
	out, err := b.git(ctx, w, nil, "diff", "--name-only", "-z", base, tree, "--")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []string{}, nil
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
}

// ownPatch excludes line offsets and blob ids, but retains all exact added and
// removed bytes, file names, mode changes and binary patches. This deliberately
// refuses ambiguous non-identical rewrites even if Git calls them a clean merge.
func (b *Broker) ownPatch(ctx context.Context, w worktree.Binding, base, tree string, paths ...string) (string, error) {
	args := append([]string{"-c", "core.quotePath=true", "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--binary", "--unified=0", base, tree, "--"}, paths...)
	out, err := b.git(ctx, w, nil, args...)
	if err != nil {
		return "", err
	}
	lines := strings.Split(out, "\n")
	keep := lines[:0]
	for _, s := range lines {
		if strings.HasPrefix(s, "index ") || strings.HasPrefix(s, "@@ ") {
			continue
		}
		keep = append(keep, s)
	}
	return strings.Join(keep, "\n"), nil
}
func (b *Broker) reconcilePlan(ctx context.Context, w worktree.Binding, s ReviewedSnapshot, ownBase, ref, tip string) (ReconcileEvidence, error) {
	r := ReconcileEvidence{OriginalBase: s.Base, OriginalTree: s.Tree, OwnBase: ownBase, Base: tip, Ref: ref, Files: append([]string{}, s.Files...)}
	if !objectID(s.Base) || !objectID(ownBase) || !objectID(s.Tree) || !objectID(tip) {
		return r, fmt.Errorf("reconcile requires immutable object ids")
	}
	if err := validFiles(s.Files); err != nil {
		return r, err
	}
	if _, err := b.git(ctx, w, nil, "merge-base", "--is-ancestor", ownBase, s.Base); err != nil {
		return r, fmt.Errorf("own base is not a task ancestor")
	}
	sort.Strings(r.Files)
	paths, err := b.changedPaths(ctx, w, ownBase, s.Tree)
	if err != nil {
		return r, err
	}
	allowed := map[string]bool{}
	for _, p := range s.Files {
		allowed[p] = true
	}
	for _, p := range paths {
		if !allowed[p] {
			return r, fmt.Errorf("unreviewed path %q", p)
		}
	}
	// Explicit-base merge includes uncommitted reviewed hunks and committed work
	// since the caller's immutable base, without consulting the mutable index.
	cmd := exec.CommandContext(ctx, "git", "-C", w.Path, "-c", "core.hooksPath=/dev/null", "merge-tree", "--write-tree", "--name-only", "-z", "--merge-base="+ownBase, tip, s.Tree)
	cmd.Env = b.literalPathGitEnv(w, nil)
	out, mergeErr := cmd.CombinedOutput()
	fields := strings.Split(string(out), "\x00")
	tree := strings.TrimSpace(fields[0])
	if objectID(tree) {
		r.Tree = tree
		r.Changed = s.Base != tip || s.Tree != tree
	}
	if mergeErr != nil {
		var conflicts []string
		for _, f := range fields[1:] {
			if f == "" {
				break
			}
			conflicts = append(conflicts, f)
		}
		if len(conflicts) == 0 {
			return r, fmt.Errorf("merge-tree: %w: %s", mergeErr, out)
		}
		sort.Strings(conflicts)
		return r, &ReconcileError{Kind: "conflicted", Paths: conflicts}
	}
	if !objectID(tree) {
		return r, fmt.Errorf("invalid merge-tree output")
	}
	r.Tree = tree
	r.ChangedPaths, err = b.changedPaths(ctx, w, s.Tree, tree)
	if err != nil {
		return r, err
	}
	oldPatch, err := b.ownPatch(ctx, w, ownBase, s.Tree)
	if err != nil {
		return r, err
	}
	newPatch, err := b.ownPatch(ctx, w, tip, tree)
	if err != nil {
		return r, err
	}
	r.PatchIdentical = oldPatch == newPatch
	r.Changed = s.Base != tip || s.Tree != tree
	// An empty replay is safe only when all touched blobs/modes match upstream.
	r.AlreadyContained, err = b.pathsEqual(ctx, w, s.Tree, tip, paths)
	if err != nil {
		return r, err
	}
	if !r.PatchIdentical && !r.AlreadyContained {
		lost := []string{}
		for _, path := range paths {
			before, err := b.ownPatch(ctx, w, ownBase, s.Tree, path)
			if err != nil {
				return r, err
			}
			after, err := b.ownPatch(ctx, w, tip, tree, path)
			if err != nil {
				return r, err
			}
			if before != after {
				lost = append(lost, path)
			}
		}
		return r, &ReconcileError{Kind: "own-hunk-loss", Paths: lost}
	}
	return r, nil
}
func (b *Broker) PrepareReconcile(ctx context.Context, w worktree.Binding, s ReviewedSnapshot, ref string) (ReconcileEvidence, error) {
	return b.PrepareReconcileFrom(ctx, w, s, s.Base, ref)
}

// PrepareReconcileFrom includes all commits since ownBase plus the uncommitted
// reviewed snapshot. ownBase must be the durable task fork point, not merge-base
// inferred anew after upstream has independently acquired task hunks.
func (b *Broker) PrepareReconcileFrom(ctx context.Context, w worktree.Binding, s ReviewedSnapshot, ownBase, ref string) (ReconcileEvidence, error) {
	if err := b.validate(ctx, w); err != nil {
		return ReconcileEvidence{}, err
	}
	current, err := b.Tree(ctx, w)
	if err != nil {
		return ReconcileEvidence{}, err
	}
	head, err := b.git(ctx, w, nil, "rev-parse", "HEAD")
	if err != nil {
		return ReconcileEvidence{}, err
	}
	if current != s.Tree || head != s.Base {
		return ReconcileEvidence{}, fmt.Errorf("reviewed tree or base changed")
	}
	tip, err := b.integrationTip(ctx, w, ref)
	if err != nil {
		return ReconcileEvidence{}, err
	}
	if !objectID(ownBase) {
		return ReconcileEvidence{}, fmt.Errorf("immutable own base required")
	}
	if _, err := b.git(ctx, w, nil, "merge-base", "--is-ancestor", ownBase, s.Base); err != nil {
		return ReconcileEvidence{}, fmt.Errorf("own base is not a task ancestor")
	}
	return b.reconcilePlan(ctx, w, s, ownBase, ref, tip)
}

// ApplyReconcile accepts only original or already-applied states, including the
// interrupted state after HEAD CAS but before index/worktree replacement. It
// never updates the integration ref. Unknown post-review edits fail closed.
func (b *Broker) ApplyReconcile(ctx context.Context, w worktree.Binding, r ReconcileEvidence) error {
	if err := b.validate(ctx, w); err != nil {
		return err
	}
	tip, err := b.integrationTip(ctx, w, r.Ref)
	if err != nil {
		return err
	}
	if tip != r.Base {
		return fmt.Errorf("integration tip advanced; prepare reconciliation again")
	}
	checked, err := b.reconcilePlan(ctx, w, ReviewedSnapshot{r.OriginalBase, r.OriginalTree, r.Files}, r.OwnBase, r.Ref, tip)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(checked, r) {
		return fmt.Errorf("reconcile evidence mismatch")
	}
	return b.applyKnownReconcile(ctx, w, r, reflect.DeepEqual(checked, r))
}

// ApplyConflict applies only a re-derived conflicted tree after its immutable
// plan is durable. The caller must prevent protected/frozen-file conflicts.
func (b *Broker) ApplyConflict(ctx context.Context, w worktree.Binding, r ReconcileEvidence, paths []string) error {
	if err := b.validate(ctx, w); err != nil {
		return err
	}
	tip, err := b.integrationTip(ctx, w, r.Ref)
	if err != nil {
		return err
	}
	if tip != r.Base {
		return fmt.Errorf("integration tip advanced")
	}
	checked, err := b.reconcilePlan(ctx, w, ReviewedSnapshot{r.OriginalBase, r.OriginalTree, r.Files}, r.OwnBase, r.Ref, tip)
	var conflict *ReconcileError
	if !errors.As(err, &conflict) || conflict.Kind != "conflicted" || !reflect.DeepEqual(conflict.Paths, paths) || !reflect.DeepEqual(checked, r) {
		return fmt.Errorf("conflict plan mismatch")
	}
	for _, path := range paths {
		for _, tree := range []string{r.OriginalTree, r.Base, r.Tree} {
			mode, e := b.git(ctx, w, nil, "ls-tree", tree, "--", path)
			if e != nil {
				return e
			}
			if !strings.HasPrefix(mode, "100644 blob ") && !strings.HasPrefix(mode, "100755 blob ") {
				return fmt.Errorf("automatic conflict requires regular text path %s", path)
			}
			blob, e := b.git(ctx, w, nil, "show", tree+":"+path)
			if e != nil {
				return e
			}
			if strings.Contains(blob, "\x00") {
				return fmt.Errorf("automatic binary conflict refused: %s", path)
			}
		}
	}
	return b.applyKnownReconcile(ctx, w, r, true)
}
func (b *Broker) applyKnownReconcile(ctx context.Context, w worktree.Binding, r ReconcileEvidence, valid bool) error {
	tip, err := b.integrationTip(ctx, w, r.Ref)
	if err != nil {
		return err
	}
	head, err := b.git(ctx, w, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	tree, err := b.Tree(ctx, w)
	if err != nil {
		return err
	}
	if head != r.OriginalBase && head != r.Base {
		return fmt.Errorf("task HEAD changed")
	}
	if tree != r.OriginalTree && tree != r.Tree {
		return fmt.Errorf("post-review worktree edits refused")
	}
	if !kernel.CanApplyReconcile(tip == r.Base, head == r.OriginalBase || head == r.Base, tree == r.OriginalTree || tree == r.Tree, valid) {
		return fmt.Errorf("reconcile state refused")
	}
	return b.applyReconcileIndex(ctx, w, r)
}
func (b *Broker) pathsEqual(ctx context.Context, w worktree.Binding, a, z string, paths []string) (bool, error) {
	if len(paths) == 0 {
		return true, nil
	}
	args := append([]string{"diff", "--no-ext-diff", "--no-textconv", "--name-only", "-z", a, z, "--"}, paths...)
	out, err := b.git(ctx, w, nil, args...)
	return out == "", err
}

// equivalentAt derives touched paths from the commit itself, never caller Files.
// This prevents an incomplete declared file list from laundering containment.
func (b *Broker) equivalentAt(ctx context.Context, w worktree.Binding, commit, tip string) (bool, error) {
	parents, err := b.git(ctx, w, nil, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return false, err
	}
	fields := strings.Fields(parents)
	if len(fields) != 2 {
		return false, nil
	} // merge/root commits require ancestry containment
	paths, err := b.changedPaths(ctx, w, fields[1], commit)
	if err != nil {
		return false, err
	}
	equal, err := b.pathsEqual(ctx, w, commit, tip, paths)
	if err != nil || equal {
		return equal, err
	}
	cherry, err := b.git(ctx, w, nil, "cherry", tip, commit, fields[1])
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(cherry) == "- "+commit, nil
}

// TargetMovedError is only emitted for pre-write target mismatch or Git's exact
// compare-and-swap expected-value refusal. Unknown outcomes use ordinary errors.
type TargetMovedError struct {
	Expected, Actual, Commit string
	NoWrite, OwnCommitKnown  bool
}

func (e *TargetMovedError) Error() string {
	return fmt.Sprintf("integration base changed from %s to %s; reconciliation required", e.Expected, e.Actual)
}

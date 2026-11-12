package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/workflow"
)

// ReadRootConfig configures a read-only external directory root (FAE-RT-016).
// Exactly one of Commit or Digest must be set.
type ReadRootConfig struct {
	Path   string   `json:"path"`
	Commit string   `json:"commit,omitempty"`
	Digest string   `json:"digest,omitempty"`
	Deny   []string `json:"deny,omitempty"`
}

// ContentPolicyConfig configures literal and regex content filters (FAE-SEC-009).
type ContentPolicyConfig struct {
	DenyLiterals []string `json:"deny_literals,omitempty"`
	DenyPatterns []string `json:"deny_patterns,omitempty"`
}

// CommitIdentityConfig configures the author, committer, and optional
// Co-authored-by trailer for commits and landings (FAE-VCS-012).
type CommitIdentityConfig struct {
	Name     string `json:"name,omitempty"`
	Email    string `json:"email,omitempty"`
	CoAuthor string `json:"co_author,omitempty"`
}

// PushConfig configures Local-mode engine-run git-push (FAE-EXT-023).
type PushConfig struct {
	Remote   string   `json:"remote"`
	Branches []string `json:"branches"`
	Mode     string   `json:"mode"`
}

// EffectsContentPolicy converts TargetConfig's content_policy and deny_paths
// into an effects.ContentPolicy.
func (c TargetConfig) EffectsContentPolicy() effects.ContentPolicy {
	return effects.ContentPolicy{
		DenyLiterals: slices.Clone(c.ContentPolicy.DenyLiterals),
		DenyPatterns: slices.Clone(c.ContentPolicy.DenyPatterns),
		DenyPaths:    slices.Clone(c.DenyPaths),
	}
}

// EffectsCommitIdentity converts TargetConfig's commit_identity into an
// *effects.CommitIdentity.
func (c TargetConfig) EffectsCommitIdentity() *effects.CommitIdentity {
	if c.CommitIdentity == nil {
		return nil
	}
	return &effects.CommitIdentity{
		Name:     c.CommitIdentity.Name,
		Email:    c.CommitIdentity.Email,
		CoAuthor: c.CommitIdentity.CoAuthor,
	}
}

func validateTargetSecurityAndRoots(c TargetConfig) error {
	if err := c.EffectsContentPolicy().Validate(); err != nil {
		return err
	}
	if id := c.CommitIdentity; id != nil {
		hasName := strings.TrimSpace(id.Name) != ""
		hasEmail := strings.TrimSpace(id.Email) != ""
		if hasName != hasEmail {
			return errors.New("commit_identity requires both name and email when either is set")
		}
		if strings.ContainsAny(id.Name, "\x00\n\r") || strings.ContainsAny(id.Email, "\x00\n\r") || strings.ContainsAny(id.CoAuthor, "\x00\n\r") {
			return errors.New("commit_identity fields must not contain newlines or NUL")
		}
	}
	if p := c.Push; p != nil {
		remote := strings.TrimSpace(p.Remote)
		if remote == "" || strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, " \t\n\r\x00") {
			return errors.New("push.remote must be a valid non-empty remote name")
		}
		if p.Mode != "fast-forward-only" {
			return errors.New(`push.mode must be "fast-forward-only"`)
		}
		if len(p.Branches) == 0 {
			return errors.New("push.branches must be non-empty")
		}
		seen := map[string]bool{}
		for _, b := range p.Branches {
			branch := strings.TrimPrefix(strings.TrimSpace(b), "refs/heads/")
			if branch == "" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, " \t\n\r~^:?*[\\\x00") || strings.Contains(branch, "..") || seen[branch] {
				return fmt.Errorf("invalid or duplicate push branch %q", b)
			}
			seen[branch] = true
		}
	}
	for name, r := range c.ReadRoots {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t\n\r/:\x00") {
			return fmt.Errorf("read_roots[%q]: invalid root name", name)
		}
		if r.Path == "" || !filepath.IsAbs(r.Path) || filepath.Clean(r.Path) != r.Path || strings.ContainsAny(r.Path, "\x00\n\r") {
			return fmt.Errorf("read_roots[%q].path: must be a clean absolute path", name)
		}
		if c.Workspace != "" && (sameOrWithinPath(c.Workspace, r.Path) || sameOrWithinPath(r.Path, c.Workspace)) {
			return fmt.Errorf("read_roots[%q].path: must be outside the target workspace", name)
		}
		hasCommit := strings.TrimSpace(r.Commit) != ""
		hasDigest := strings.TrimSpace(r.Digest) != ""
		if hasCommit == hasDigest {
			return fmt.Errorf("read_roots[%q]: must set exactly one of commit or digest", name)
		}
		for i, d := range r.Deny {
			if !workflow.ValidGlob(d) {
				return fmt.Errorf("read_roots[%q].deny[%d]: invalid glob", name, i)
			}
		}
	}
	return nil
}

func sameOrWithinPath(parent, child string) bool {
	p := filepath.Clean(parent)
	c := filepath.Clean(child)
	if p == c {
		return true
	}
	rel, err := filepath.Rel(p, c)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// VerifyReadRoot verifies a configured ReadRootConfig on disk (FAE-RT-016):
//   - refuses any symlink inside the root that points outside the root;
//   - verifies git rev-parse HEAD (and clean status) when Commit is set, or
//     deterministic SHA-256 tree digest when Digest is set;
//   - returns "read-root-changed: <name>" if the identity differs from the
//     declared or pinned one.
func VerifyReadRoot(ctx context.Context, name string, r ReadRootConfig, targetDenyPaths []string) (string, error) {
	physical, err := filepath.EvalSymlinks(r.Path)
	if err != nil {
		return "", fmt.Errorf("read_roots[%s]: %w", name, err)
	}
	st, err := os.Stat(physical)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("read_roots[%s]: path is not a directory", name)
	}
	if err := checkReadRootSymlinks(physical); err != nil {
		return "", fmt.Errorf("read_roots[%s]: %w", name, err)
	}
	if strings.TrimSpace(r.Commit) != "" {
		cmd := exec.CommandContext(ctx, "git", "--no-optional-locks", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", physical, "rev-parse", "--verify", "HEAD^{commit}")
		var out, errBuf bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("read-root-changed: %s (git rev-parse failed: %v)", name, strings.TrimSpace(errBuf.String()))
		}
		actual := strings.TrimSpace(out.String())
		if actual != strings.TrimSpace(r.Commit) {
			return actual, fmt.Errorf("read-root-changed: %s", name)
		}
		statusCmd := exec.CommandContext(ctx, "git", "--no-optional-locks", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", physical, "status", "--porcelain")
		if statusOut, err := statusCmd.Output(); err == nil && len(bytes.TrimSpace(statusOut)) > 0 {
			return actual, fmt.Errorf("read-root-changed: %s", name)
		}
		return actual, nil
	}
	actualDigest, err := ComputeReadRootDigest(physical, r.Deny, targetDenyPaths)
	if err != nil {
		return "", fmt.Errorf("read_roots[%s]: %w", name, err)
	}
	wantDigest := strings.TrimPrefix(strings.TrimSpace(r.Digest), "sha256:")
	if !strings.EqualFold(actualDigest, wantDigest) {
		return actualDigest, fmt.Errorf("read-root-changed: %s", name)
	}
	return actualDigest, nil
}

func checkReadRootSymlinks(root string) error {
	return filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(full)
			if err != nil {
				return fmt.Errorf("broken symlink %q inside read root: %w", full, err)
			}
			rel, err := filepath.Rel(root, target)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("symlink %q inside read root points outside root (%q)", full, target)
			}
		}
		return nil
	})
}

// ComputeReadRootDigest computes a deterministic SHA-256 tree digest of all
// regular files under rootPath not excluded by deny or targetDenyPaths (FAE-RT-016).
func ComputeReadRootDigest(rootPath string, deny []string, targetDenyPaths []string) (string, error) {
	physical, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		physical = filepath.Clean(rootPath)
	}
	if err := checkReadRootSymlinks(physical); err != nil {
		return "", err
	}
	var rels []string
	walkErr := filepath.WalkDir(physical, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if full == physical || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(physical, full)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		for _, pat := range deny {
			if workflow.MatchGlob(pat, relSlash) || workflow.MatchGlobFold(pat, relSlash) {
				return nil
			}
		}
		if workflow.MatchDenyPathIndex(targetDenyPaths, relSlash) >= 0 {
			return nil
		}
		rels = append(rels, relSlash)
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	slices.Sort(rels)
	h := sha256.New()
	for _, relSlash := range rels {
		full := filepath.Join(physical, filepath.FromSlash(relSlash))
		data, err := os.ReadFile(full)
		if err != nil {
			return "", err
		}
		fileSum := sha256.Sum256(data)
		_, _ = fmt.Fprintf(h, "%s\x00%s\n", relSlash, hex.EncodeToString(fileSum[:]))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkScopeDenyPaths refuses any file in files or testFile that matches denyPaths (FAE-SEC-009).
func checkScopeDenyPaths(files []string, testFile string, denyPaths []string) error {
	if len(denyPaths) == 0 {
		return nil
	}
	for _, f := range files {
		if idx := workflow.MatchDenyPathIndex(denyPaths, f); idx >= 0 {
			return &execution.ScopeRefusal{Detail: fmt.Sprintf("path %q refused by deny_paths[%d]", f, idx)}
		}
	}
	if testFile != "" {
		if idx := workflow.MatchDenyPathIndex(denyPaths, testFile); idx >= 0 {
			return &execution.ScopeRefusal{Detail: fmt.Sprintf("path %q refused by deny_paths[%d]", testFile, idx)}
		}
	}
	return nil
}

// contentPolicySecretForms builds secretForm entries for a target's
// ContentPolicyConfig so they are redacted by secretValues (FAE-SEC-009).
func contentPolicySecretForms(cp ContentPolicyConfig) []secretForm {
	var forms []secretForm
	for _, lit := range cp.DenyLiterals {
		if strings.TrimSpace(lit) == "" {
			continue
		}
		raw := []byte(lit)
		forms = append(forms, secretForm{text: raw, fold: true, raw: true})
		for _, text := range credentialForms(raw) {
			forms = append(forms, secretForm{text: text, fold: true})
		}
	}
	for _, pat := range cp.DenyPatterns {
		if strings.TrimSpace(pat) == "" {
			continue
		}
		if re, err := regexp.Compile("(?i)" + pat); err == nil {
			forms = append(forms, secretForm{pattern: re})
		}
	}
	return forms
}

func workflowReferencedReadRoots(doc workflow.Document) []string {
	var names []string
	for _, r := range doc.Roles {
		for _, n := range r.ReadRoots {
			if n != "" && !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	for _, s := range doc.Stages {
		for _, n := range workflow.StageReadRoots(doc, "", s) {
			if n != "" && !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	slices.Sort(names)
	return names
}

func validateAdmissionReads(doc workflow.Document, taskReads *workflow.ReadPolicy) error {
	if taskReads == nil {
		return nil
	}
	if err := workflow.ValidateReadPolicy(taskReads); err != nil {
		return err
	}
	roleNames := make([]string, 0, len(doc.Roles))
	for name := range doc.Roles {
		roleNames = append(roleNames, name)
	}
	slices.Sort(roleNames)
	for _, roleName := range roleNames {
		role := doc.Roles[roleName]
		if err := workflow.ValidateReadPolicyNarrowing(role.Reads, taskReads); err != nil {
			return fmt.Errorf("role %s: %w", roleName, err)
		}
	}
	return nil
}

func (d *Daemon) verifyAdmissionTargetRoots(ctx context.Context, p AdmitParams) (map[string]string, []string, error) {
	if p.TargetConfig == "" {
		return nil, nil, nil
	}
	target, err := d.pinnedTarget(ctx, p)
	if err != nil || target == nil {
		return nil, nil, err
	}
	for _, f := range append(append([]string{}, p.DeclaredFiles...), p.ReadOnlyFiles...) {
		if idx := workflow.MatchDenyPathIndex(target.DenyPaths, f); idx >= 0 {
			return nil, nil, fmt.Errorf("file %q matches target deny_paths (FAE-SEC-009): deny_paths[%d]", f, idx)
		}
		if workflow.MatchesAnyGlob(f, target.DenyPaths) {
			return nil, nil, fmt.Errorf("file %q matches target deny_paths (FAE-SEC-009)", f)
		}
	}
	var pinnedRoots map[string]string
	if len(target.ReadRoots) > 0 {
		pinnedRoots = make(map[string]string, len(target.ReadRoots))
		names := make([]string, 0, len(target.ReadRoots))
		for name := range target.ReadRoots {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			resolved, err := VerifyReadRoot(ctx, name, target.ReadRoots[name], target.DenyPaths)
			if err != nil {
				return nil, nil, err
			}
			pinnedRoots[name] = resolved
		}
	}
	return pinnedRoots, slices.Clone(target.DenyPaths), nil
}

// lockReadRootsReadOnly strips write bits on files and directories inside the
// given read roots during execution and returns a restore callback (FAE-RT-016).
func lockReadRootsReadOnly(roots []string) func() { return func() {} } // child read-only namespace mounts replace shared chmod.

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/xleelz/fae/workflow"
)

// The repository block lists at most this many tracked paths and this much
// path text (each path counted with one separator byte), sorted.
const (
	maxRepositoryPaths        = 5000
	maxRepositoryListingBytes = 256 << 10
	// git's own output is read up to this bound and drained beyond it.
	maxRepositoryGitOutput = 8 << 20
	repositoryGitTimeout   = 30 * time.Second
)

// repositoryListing is the prompt block that shows a turn the worktree's
// layout: its tracked paths, sorted, with truncated set when a cap cut it.
type repositoryListing struct {
	Kind      string   `json:"kind"`
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated"`
}

// listRepository returns the worktree's repository block from `git ls-files
// -z`, run without system or global configuration. Any git failure refuses the
// turn: a partial listing must never look complete.
func listRepository(ctx context.Context, root, logDirectory string) (json.RawMessage, error) {
	return listRepositoryFiltered(ctx, root, logDirectory, nil, nil)
}

func listRepositoryFiltered(ctx context.Context, root, logDirectory string, policy *workflow.ReadPolicy, denyPaths []string) (json.RawMessage, error) {
	git, e := repositoryGit(os.Getenv("PATH"), logDirectory)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, repositoryGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "--no-pager", "-c", "core.fsmonitor=false", "-C", root, "ls-files", "-z")
	// The worktree must itself be a repository: never list a parent one.
	cmd.Env = append(repositoryGitEnv(os.Environ()), "GIT_CEILING_DIRECTORIES="+filepath.Dir(root))
	cmd.Dir = root
	var out, stderr bytes.Buffer
	cut := &boundedLog{destination: &out, remaining: maxRepositoryGitOutput}
	cmd.Stdout = cut
	cmd.Stderr = &boundedLog{destination: &stderr, remaining: 4 << 10}
	if e = cmd.Run(); e != nil {
		return nil, fmt.Errorf("repository listing failed: %w: %s", e, strings.TrimSpace(stderr.String()))
	}
	entries := strings.Split(out.String(), "\x00")
	// A complete listing ends with a separator; a cut one ends mid-path.
	partial := entries[len(entries)-1] != ""
	entries = entries[:len(entries)-1]
	listing := repositoryBlockFiltered(entries, policy, denyPaths)
	listing.Truncated = listing.Truncated || partial || cut.remaining == 0
	return json.Marshal(listing)
}

// repositoryBlock sorts and deduplicates paths and applies the path-count and
// byte caps.
func repositoryBlock(paths []string) repositoryListing {
	return repositoryBlockFiltered(paths, nil, nil)
}

func repositoryBlockFiltered(paths []string, policy *workflow.ReadPolicy, denyPaths []string) repositoryListing {
	if policy != nil || len(denyPaths) > 0 {
		g := &ReadGuard{Policy: policy, DenyPaths: denyPaths}
		paths = g.FilterRepositoryPaths(paths)
	}
	sorted := slices.Clone(paths)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	listing := repositoryListing{Kind: "repository", Files: []string{}}
	size := 0
	for _, p := range sorted {
		if p == "" {
			continue
		}
		if len(listing.Files) == maxRepositoryPaths || size+len(p)+1 > maxRepositoryListingBytes {
			listing.Truncated = true
			break
		}
		listing.Files = append(listing.Files, p)
		size += len(p) + 1
	}
	return listing
}

// repositoryGit finds git on PATH, skipping the role's command shims: the
// runtime puts a refusing git wrapper first on the bridge's PATH, in the
// session's log directory, for the model's process.
func repositoryGit(path, logDirectory string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) || filepath.Base(dir) == "command-shims" || logDirectory != "" && within(logDirectory, dir) {
			continue
		}
		candidate := filepath.Join(dir, "git")
		if st, e := os.Stat(candidate); e == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("repository listing needs git outside the role command shims on PATH")
}

func within(parent, path string) bool {
	rel, e := filepath.Rel(parent, path)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// repositoryGitEnv drops inherited Git redirections and ignores system and
// global configuration, without a pager or prompts.
func repositoryGitEnv(environ []string) []string {
	env := []string{}
	for _, entry := range environ {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
}

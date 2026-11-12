package daemon

import (
	"context"
	"errors"

	"github.com/xleelz/fae/effects"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// goTestName is the pinned RED identity the broker can anchor exactly.
var goTestName = regexp.MustCompile(`^Test[A-Za-z0-9_]*$`)

// LiveTaskConfig pins each task's declared scope and genuine RED identity.
// Overrides are part of the instance configuration hash, never model supplied.
// A task without an override, on an instance without files, derives its
// scope from its own admission and evidence instead (resolveLiveTaskScope),
// where the engine's RED gate, freeze and compare checks hold the
// model-supplied identity.
type LiveTaskConfig struct {
	Files           []string `json:"files"`
	TestFile        string   `json:"test_file"`
	TestID          string   `json:"test_id"`
	ExpectedFailure string   `json:"expected_failure"`
}

func validateLiveTasks(tasks map[string]LiveTaskConfig) error {
	for id, task := range tasks {
		if id == "" || len(task.Files) == 0 || task.TestFile == "" || task.TestID == "" || task.ExpectedFailure == "" {
			return errors.New("task override requires ID, files and complete RED identity")
		}
		// Go shapes narrow to a Go test name at run time; script and runner
		// shapes take any literal identifier that is never a pattern.
		if !goTestName.MatchString(task.TestID) && !effects.SafeTestID(task.TestID) {
			return errors.New("task RED test id must be a Go test name or a literal test identifier")
		}
		seen := map[string]bool{}
		for _, name := range task.Files {
			if !DeclaredPathSafe(name) || seen[name] {
				return errors.New("task override has unsafe or duplicate file")
			}
			seen[name] = true
		}
		if !seen[task.TestFile] {
			return errors.New("task RED file must be declared")
		}
	}
	return nil
}

// formatDebt counts formatter-listed files the task changed between base and
// tree (Appendix C step 12: no new debt on changed items) plus one for a
// failed formatter run. Debt already present at the base is not the task's.
func formatDebt(ctx context.Context, worktree, base, tree, listed string, exit int) (int, error) {
	if base == "" || tree == "" {
		return 0, errors.New("format debt requires the task base and tree")
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "--no-renames", base, tree)
	cmd.Dir = worktree
	cmd.Env = cleanGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	changed := map[string]bool{}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		changed[name] = name != ""
	}
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return 0, err
	}
	debt := 0
	for _, path := range strings.Fields(listed) {
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		if rel, e := filepath.Rel(root, path); e == nil && changed[filepath.ToSlash(rel)] {
			debt++
		}
	}
	if exit != 0 {
		debt++
	}
	return debt, nil
}

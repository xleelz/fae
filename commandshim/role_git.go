package commandshim

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

func runRoleGit(ctx context.Context, s RoleShims, h HeavyOp) (int, error) {
	cwd := h.Dir
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return 126, err
		}
	}
	call, err := parseAccountedGit(cwd, h.Argv[1:], h.Env)
	if err != nil {
		return Refused, err
	}
	if gitExternalSubcommands[call.sub] {
		if s.Local {
			return LocalModeRefused, &LocalRefusal{"role git remote commands require engine authority"}
		}
		return Refused, refuse("role git remote commands require engine authority")
	}
	if !slices.Contains([]string{"rev-parse", "status", "diff", "log", "show", "ls-files"}, call.sub) {
		return Refused, refuse("role sessions allow read-only git queries; the engine's VCS broker commits")
	}
	worktree, err := filepath.EvalSymlinks(s.Worktree)
	if err != nil {
		return Refused, err
	}
	dir, err := filepath.EvalSymlinks(call.dir)
	if err != nil {
		return Refused, err
	}
	rel, err := filepath.Rel(worktree, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return Refused, refuse("git query outside the session worktree")
	}
	// The accounted policy rejects file outputs, executable hooks/config, and
	// alternate repository paths, and pins the query to the measured repository.
	policy := Policy{Worktree: s.Worktree, CWD: cwd, Local: s.Local}
	argv, err := policy.PrepareAccounted(append([]string{s.Tools["git"]}, h.Argv[1:]...), h.Env)
	if err != nil {
		return Refused, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = accountedGitExecEnv(h.Env)
	cmd.Stdin = h.Stdin
	cmd.Stdout = h.Stdout
	cmd.Stderr = h.Stderr
	if err = cmd.Run(); err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), err
	}
	return 126, err
}

package controller

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type OverlapHold struct {
	Task                                string
	Since                               time.Time
	Critical, TopNearLimit, QueryFailed bool
}

func SourceRevision(binary string) (string, error) {
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		return "", err
	}
	revision := ""
	clean := false
	for _, s := range info.Settings {
		if s.Key == "vcs.modified" && s.Value != "false" {
			return "", errors.New("dirty binary source")
		}
		if s.Key == "vcs.modified" && s.Value == "false" {
			clean = true
		}
		if s.Key == "vcs.revision" {
			revision = s.Value
		}
	}
	if revision == "" || !clean {
		return "", errors.New("binary lacks VCS revision")
	}
	return revision, nil
}
func Git(ctx context.Context, workspace string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", workspace}, args...)...)
	// Ignore inherited Git redirections when operating on the bound workspace.
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	b, e := cmd.Output()
	if slices.Contains(args, "-z") {
		return string(b), e
	}
	return strings.TrimSpace(string(b)), e
}

// BuildGated compiles a detached clone, leaving the operator checkout untouched.
// The compile passes host admission and holds the host heavy-op lease; it runs
// with clamped build jobs and its scratch on disk under storage, never in the
// RAM-backed system temporary directory (FAE-HOST-020). The caller owns the
// returned directory until deployment and rollback finish.
func BuildGated(ctx context.Context, lease BuildLease, workspace, ref, gated string, storage ...string) (binary, directory, tip string, err error) {
	if !filepath.IsAbs(lease.Ledger) {
		err = errors.New("controller build requires the instance's host heavy-op ledger")
		return
	}
	if len(storage) == 0 || !filepath.IsAbs(storage[0]) {
		err = errors.New("controller build requires on-disk deployment storage")
		return
	}
	if !strings.HasPrefix(gated, "refs/fae/gated/") {
		err = errors.New("build requires refs/fae/gated reference")
		return
	}
	tip, err = Git(ctx, workspace, "rev-parse", "--verify", gated+"^{commit}")
	if err != nil {
		return
	}
	requested, e := Git(ctx, workspace, "rev-parse", "--verify", ref+"^{commit}")
	if e != nil {
		err = e
		return
	}
	if requested != tip {
		err = errors.New("build ref is not gated tip")
		return
	}
	root := storage[0]
	if err = os.MkdirAll(root, 0700); err != nil {
		return
	}
	directory, err = os.MkdirTemp(root, "fae-deployment-")
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			os.RemoveAll(directory)
		}
	}()
	checkout := filepath.Join(directory, "source")
	_, err = Git(ctx, workspace, "clone", "--no-checkout", "--no-hardlinks", workspace, checkout)
	if err != nil {
		return
	}
	_, err = Git(ctx, checkout, "checkout", "--detach", tip)
	if err != nil {
		return
	}
	binary = filepath.Join(directory, "fae")
	scratch := filepath.Join(directory, "tmp")
	if err = os.Mkdir(scratch, 0700); err != nil {
		return
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-p", fmt.Sprint(packageBuildJobs), "-buildvcs=true", "-o", binary, "./cmd/fae")
	cmd.Dir = checkout
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") && !strings.HasPrefix(v, "CGO_") && !strings.HasPrefix(v, "GOFLAGS=") && !strings.HasPrefix(v, "GOWORK=") && !strings.HasPrefix(v, "GOENV=") && !strings.HasPrefix(v, "TMPDIR=") && !strings.HasPrefix(v, "GOTMPDIR=") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "GOENV=off", "GOWORK=off", "GOFLAGS=", "TMPDIR="+scratch, "GOTMPDIR="+scratch)
	if err = lease.hold(ctx, "build", func() error {
		if b, e := cmd.CombinedOutput(); e != nil {
			return fmt.Errorf("build gated source: %w: %s", e, b)
		}
		return nil
	}); err != nil {
		return
	}
	if err = errors.Join(os.RemoveAll(checkout), os.RemoveAll(scratch)); err != nil {
		return
	}
	revision, e := SourceRevision(binary)
	if e != nil {
		err = e
		return
	}
	if revision != tip {
		err = errors.New("built binary revision mismatch")
	}
	return
}

// FastForward is idempotent and uses compare-and-swap; divergence leaves ref intact.
func FastForward(ctx context.Context, workspace, ref, tip string) error {
	if !strings.HasPrefix(ref, "refs/heads/") {
		return errors.New("integration reference must be local branch")
	}
	old, e := Git(ctx, workspace, "rev-parse", "--verify", ref+"^{commit}")
	if e != nil {
		return e
	}
	if old == tip {
		return nil
	}
	if _, e = Git(ctx, workspace, "merge-base", "--is-ancestor", old, tip); e != nil {
		return errors.New("integration reference diverged from deployed tip")
	}
	_, e = Git(ctx, workspace, "update-ref", ref, tip, old)
	return e
}

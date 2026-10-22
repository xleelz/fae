// Package effects executes narrowly authorized host and Git effects in registered
// task worktrees. Callers must hold a durable task/resource lease throughout.
package effects

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/worktree"
)

type Broker struct {
	Manager                       *worktree.Manager
	Allowed                       [][]string
	Environment                   []string
	MaxOutput                     int
	ProcessRegistry, ProcessOwner string
	ProcessSession                string
}
type Command struct {
	Argv                    []string
	Timeout                 time.Duration
	TestID, ExpectedFailure string
}
type RunEvidence struct {
	FreshBuild      bool     `json:"fresh_build"`
	Stderr          string   `json:"stderr"`
	ExitCode        int      `json:"exit_code"`
	ExecutedCount   int      `json:"executed_count"`
	MatchedCount    int      `json:"matched_count"`
	ExpectedFailure bool     `json:"expected_failure"`
	RedPassed       bool     `json:"red_passed"`
	OutputDigest    string   `json:"output_digest"`
	Tree            string   `json:"tree"`
	Output          string   `json:"output"`
	Truncated       bool     `json:"truncated"`
	Environment     []string `json:"environment"`
}

func (b *Broker) validate(ctx context.Context, w worktree.Binding) error {
	if b.Manager == nil {
		return fmt.Errorf("manager required")
	}
	return b.Manager.Verify(ctx, w)
}
func cleanEnv(extra []string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir(), "LANG=C", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	for _, e := range extra {
		key, _, ok := strings.Cut(e, "=")
		if ok && (key == "GOCACHE" || key == "GOMODCACHE" || key == "GOPATH" || key == "GOTOOLCHAIN" || key == "GOPROXY" || key == "GOSUMDB") {
			env = append(env, e)
		}
	}
	return env
}

type bounded struct {
	mu        sync.Mutex
	data      []byte
	digest    io.Writer
	limit     int
	truncated bool
}

func (w *bounded) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.digest.Write(p)
	n := len(p)
	room := w.limit - len(w.data)
	if len(p) > room {
		w.truncated = true
		p = p[:room]
	}
	w.data = append(w.data, p...)
	return n, nil
}

func (b *Broker) Run(ctx context.Context, w worktree.Binding, c Command) (RunEvidence, error) {
	var r RunEvidence
	if err := b.validate(ctx, w); err != nil {
		return r, err
	}
	allowed := false
	for _, v := range b.Allowed {
		if reflect.DeepEqual(v, c.Argv) {
			allowed = true
		}
	}
	if !allowed || len(c.Argv) == 0 || c.Timeout <= 0 || c.Timeout > 24*time.Hour {
		return r, fmt.Errorf("command vector or timeout refused")
	}
	argv, err := (commandshim.Policy{Worktree: w.Path, CWD: w.Path, CodeRole: true, LeaseHeld: true, Local: true, RAMRoots: []string{"/tmp", "/var/tmp", "/dev/shm"}}).Prepare(c.Argv)
	if err != nil {
		return r, err
	}
	before, err := b.Tree(ctx, w)
	if err != nil {
		return r, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	// Resolve before PATH wrappers are installed: only this admitted invocation bypasses them.
	binary, err := exec.LookPath(argv[0])
	if err != nil {
		return r, err
	}
	shimRoot, err := os.MkdirTemp(filepath.Dir(w.Path), "command-policy-")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(shimRoot)
	shims, err := commandshim.InstallDenials(shimRoot)
	if err != nil {
		return r, err
	}
	cmd := exec.CommandContext(ctx, binary, argv[1:]...)
	cmd.Dir = w.Path
	cmd.Env = commandshim.PrependPath(cleanEnv(b.Environment), shims)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = time.Second
	h := sha256.New()
	limit := b.MaxOutput
	if limit <= 0 {
		limit = 4 << 20
	}
	out := &bounded{digest: h, limit: limit}
	cmd.Stdout = out
	errHash := sha256.New()
	errOut := &bounded{digest: errHash, limit: limit}
	cmd.Stderr = errOut
	registry, owner := b.ProcessRegistry, b.ProcessOwner
	temporaryRegistry, retiredRegistry := false, false
	if registry == "" {
		registry, err = os.MkdirTemp("", "fae-command-owner-")
		if err != nil {
			return r, err
		}
		temporaryRegistry = true
		createdRegistry := registry
		defer func() {
			if retiredRegistry {
				_ = os.RemoveAll(createdRegistry)
			} else {
				// A cancelled launch can fail before writing any ownership
				// record. Remove only the empty directory in that case;
				// unresolved records/receipts must survive for investigation.
				_ = os.Remove(createdRegistry)
			}
		}()
		registry, err = filepath.EvalSymlinks(registry)
		if err != nil {
			return r, err
		}
		if err = os.Chmod(registry, 0700); err != nil {
			return r, err
		}
		owner = registry
	}
	var record *processowner.Record
	record, err = processowner.StartLinked(cmd, registry, owner, b.ProcessSession)
	if err == nil {
		err = processowner.Wait(cmd, record)
		if retireErr := processowner.Retire(record); retireErr != nil {
			return r, fmt.Errorf("host process ownership unresolved in %s: %w", registry, retireErr)
		}
	}

	if temporaryRegistry && record != nil {
		retiredRegistry = true
	}
	r.ExitCode = 0
	if err != nil {
		var exit *exec.ExitError
		if !asExit(err, &exit) {
			return r, err
		}
		r.ExitCode = exit.ExitCode()
	}
	if ctx.Err() != nil {
		return r, ctx.Err()
	}
	r.Output = string(out.data)
	r.Stderr = string(errOut.data)
	r.Truncated = out.truncated || errOut.truncated || len(out.data)+len(errOut.data) > limit
	if r.Truncated {
		return r, fmt.Errorf("command output exceeded bound; evidence refused")
	}
	combined := sha256.Sum256(append(h.Sum(nil), errHash.Sum(nil)...))
	r.OutputDigest = hex.EncodeToString(combined[:])
	r.Tree = before
	r.Environment = cmd.Env
	after, err := b.Tree(ctx, w)
	if err != nil {
		return r, err
	}
	if after != before {
		return r, fmt.Errorf("host command mutated source tree")
	}
	if len(c.Argv) > 1 && filepath.Base(c.Argv[0]) == "go" && c.Argv[1] == "test" {
		hasJSON, uncached := false, false
		for _, arg := range c.Argv[2:] {
			hasJSON = hasJSON || arg == "-json"
			uncached = uncached || arg == "-count=1"
		}
		if !hasJSON || !uncached {
			return r, fmt.Errorf("Go test evidence requires -json -count=1")
		}
		if r.Truncated {
			return r, fmt.Errorf("test output exceeded bound; evidence refused")
		}
		r.FreshBuild = observedCompile(r.Output, r.Stderr, w.Path, c.Argv)
		if err := parseTests(&r, c); err != nil {
			return r, err
		}
	}
	return r, nil
}
func asExit(err error, p **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*p = e
	}
	return ok
}
func parseTests(r *RunEvidence, c Command) error {
	scan := bufio.NewScanner(strings.NewReader(r.Output))
	scan.Buffer(make([]byte, 4096), 4<<20)
	started := map[string]bool{}
	ended := map[string]string{}
	var testOutput strings.Builder
	for scan.Scan() {
		var e struct{ Action, Package, Test, Output string }
		if err := json.Unmarshal(scan.Bytes(), &e); err != nil {
			return fmt.Errorf("malformed go test JSON: %w", err)
		}
		key := e.Package + "/" + e.Test
		if e.Test == "" {
			continue
		}
		if e.Action == "run" {
			started[key] = true
		}
		if e.Action == "pass" || e.Action == "fail" || e.Action == "skip" {
			ended[key] = e.Action
		}
		if e.Test == c.TestID {
			testOutput.WriteString(e.Output)
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	for key := range started {
		end := ended[key]
		if end == "" {
			return fmt.Errorf("incomplete test stream")
		}
		if end == "skip" {
			continue
		}
		r.ExecutedCount++
		if strings.HasSuffix(key, "/"+c.TestID) && c.TestID != "" {
			r.MatchedCount++
			r.RedPassed = end == "pass"
		}
	}
	r.ExpectedFailure = c.ExpectedFailure != "" && r.ExitCode != 0 && r.MatchedCount == 1 && !r.RedPassed && strings.Contains(testOutput.String(), c.ExpectedFailure)
	return nil
}

func (b *Broker) git(ctx context.Context, w worktree.Binding, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", w.Path}, args...)...)
	cmd.Env = append(cleanEnv(b.Environment), env...)
	var out []byte
	var err error
	if len(args) > 0 && args[0] == "update-ref" && b.ProcessRegistry != "" && b.ProcessSession != "" {
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		record, e := processowner.StartLinked(cmd, b.ProcessRegistry, b.ProcessOwner, b.ProcessSession)
		err = e
		if e == nil {
			err = processowner.Wait(cmd, record)
			err = errors.Join(err, processowner.Retire(record))
		}
		out = output.Bytes()
	} else {
		out, err = cmd.CombinedOutput()
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, out)
	}
	return strings.TrimSpace(string(out)), nil
}
func (b *Broker) index(ctx context.Context, w worktree.Binding, files []string) (string, error) {
	f, err := os.CreateTemp("", "fae-index-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	defer os.Remove(name)
	defer os.Remove(name + ".lock")
	env := []string{"GIT_INDEX_FILE=" + name}
	if _, err = b.git(ctx, w, env, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	args := []string{"add", "--all", "--"}
	if files == nil {
		args = append(args, ".")
	} else {
		args = append(args, files...)
	}
	if _, err = b.git(ctx, w, env, args...); err != nil {
		return "", err
	}
	return b.git(ctx, w, env, "write-tree")
}
func (b *Broker) Tree(ctx context.Context, w worktree.Binding) (string, error) {
	if err := b.validate(ctx, w); err != nil {
		return "", err
	}
	return b.index(ctx, w, nil)
}

type ReviewedSnapshot struct {
	Base, Tree string
	Files      []string
}
type CommitEvidence struct {
	SHA, Tree string
	Files     []string
}
type IntegrationEvidence struct {
	SHA, ContainedBy string
	Landed           bool
}

func validFiles(files []string) error {
	if len(files) == 0 {
		return fmt.Errorf("empty reviewed files")
	}
	seen := map[string]bool{}
	for _, f := range files {
		if f == "" || filepath.IsAbs(f) || filepath.Clean(f) != f || f == ".." || strings.HasPrefix(f, "../") || strings.Contains(f, "\x00") || strings.HasPrefix(f, ".git/") || f == ".git" || seen[f] {
			return fmt.Errorf("invalid reviewed path %q", f)
		}
		seen[f] = true
	}
	return nil
}
func (b *Broker) Snapshot(ctx context.Context, w worktree.Binding, files []string) (ReviewedSnapshot, error) {
	var s ReviewedSnapshot
	if err := b.validate(ctx, w); err != nil {
		return s, err
	}
	if err := validFiles(files); err != nil {
		return s, err
	}
	for _, f := range files {
		p := filepath.Join(w.Path, f)
		if stat, err := os.Lstat(p); err == nil && stat.IsDir() {
			return s, fmt.Errorf("reviewed directories refused")
		}
	}
	base, err := b.git(ctx, w, nil, "rev-parse", "HEAD")
	if err != nil {
		return s, err
	}
	tree, err := b.index(ctx, w, files)
	if err != nil {
		return s, err
	}
	whole, err := b.Tree(ctx, w)
	if err != nil {
		return s, err
	}
	if whole != tree {
		return s, fmt.Errorf("unreviewed changes outside declared files")
	}
	s = ReviewedSnapshot{base, tree, append([]string{}, files...)}
	sort.Strings(s.Files)
	return s, nil
}

type PreparedCommit struct {
	SHA      string
	Snapshot ReviewedSnapshot
}

func (b *Broker) PrepareCommit(ctx context.Context, w worktree.Binding, s ReviewedSnapshot, message string) (PreparedCommit, error) {
	var r PreparedCommit
	if strings.TrimSpace(message) == "" {
		return r, fmt.Errorf("empty commit message")
	}
	now, err := b.Snapshot(ctx, w, s.Files)
	if err != nil {
		return r, err
	}
	if now.Base != s.Base || now.Tree != s.Tree {
		return r, fmt.Errorf("reviewed tree or base changed")
	}
	// Plumbing writes exactly the reviewed tree, never the mutable shared index.
	cmd := exec.CommandContext(ctx, "git", "-C", w.Path, "-c", "core.hooksPath=/dev/null", "commit-tree", s.Tree, "-p", s.Base)
	cmd.Env = append(cleanEnv(b.Environment), "GIT_AUTHOR_NAME=Fae", "GIT_AUTHOR_EMAIL=fae@localhost", "GIT_COMMITTER_NAME=Fae", "GIT_COMMITTER_EMAIL=fae@localhost")
	cmd.Stdin = strings.NewReader(message + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return r, fmt.Errorf("commit-tree: %w: %s", err, out)
	}
	sha := strings.TrimSpace(string(out))
	return PreparedCommit{SHA: sha, Snapshot: s}, nil
}

// Commit preserves the existing one-call API; durable callers journal the
// prepared SHA before ApplyCommit performs its single conditional ref write.
func (b *Broker) Commit(ctx context.Context, w worktree.Binding, s ReviewedSnapshot, message string) (CommitEvidence, error) {
	p, e := b.PrepareCommit(ctx, w, s, message)
	if e != nil {
		return CommitEvidence{}, e
	}
	return b.ApplyCommit(ctx, w, p)
}
func (b *Broker) ApplyCommit(ctx context.Context, w worktree.Binding, p PreparedCommit) (CommitEvidence, error) {
	s, e := b.Snapshot(ctx, w, p.Snapshot.Files)
	if e != nil {
		return CommitEvidence{}, e
	}
	if s.Base != p.Snapshot.Base || s.Tree != p.Snapshot.Tree {
		return CommitEvidence{}, fmt.Errorf("prepared commit source changed")
	}
	tree, e := b.git(ctx, w, nil, "rev-parse", p.SHA+"^{tree}")
	if e != nil || tree != s.Tree {
		return CommitEvidence{}, fmt.Errorf("prepared commit tree mismatch")
	}
	parent, e := b.git(ctx, w, nil, "rev-parse", p.SHA+"^")
	if e != nil || parent != s.Base {
		return CommitEvidence{}, fmt.Errorf("prepared commit parent mismatch")
	}
	if e = b.applyCommitIndex(ctx, w, p, true); e != nil {
		return CommitEvidence{}, e
	}
	return CommitEvidence{p.SHA, s.Tree, append([]string{}, s.Files...)}, nil
}

// Integrate atomically advances an un-checked-out integration ref. Changed bases
// use explicit reconciliation; ancestry, patch and touched-blob containment are accepted.
func (b *Broker) Integrate(ctx context.Context, w worktree.Binding, c CommitEvidence, ref, expectedBase string) (IntegrationEvidence, error) {
	var r IntegrationEvidence
	if err := b.validate(ctx, w); err != nil {
		return r, err
	}
	if !strings.HasPrefix(ref, "refs/heads/fae/") || strings.HasPrefix(ref, "refs/heads/fae/task/") {
		return r, fmt.Errorf("integration ref must be an owned fae branch")
	}
	if _, err := b.git(ctx, w, nil, "check-ref-format", ref); err != nil {
		return r, err
	}
	list, err := b.git(ctx, w, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return r, err
	}
	if strings.Contains("\n"+list+"\n", "\nbranch "+ref+"\n") {
		return r, fmt.Errorf("integration ref is checked out")
	}
	tip, err := b.git(ctx, w, nil, "rev-parse", "--verify", ref)
	if err != nil {
		return r, err
	}
	tree, err := b.git(ctx, w, nil, "rev-parse", c.SHA+"^{tree}")
	if err != nil || tree != c.Tree {
		return r, fmt.Errorf("commit evidence mismatch")
	}
	if _, err = b.git(ctx, w, nil, "merge-base", "--is-ancestor", c.SHA, tip); err == nil {
		return IntegrationEvidence{c.SHA, tip, true}, nil
	}
	contained, err := b.equivalentAt(ctx, w, c.SHA, tip)
	if err != nil {
		return r, err
	}
	if contained {
		return IntegrationEvidence{c.SHA, tip, true}, nil
	}
	if tip != expectedBase {
		head, e := b.git(ctx, w, nil, "rev-parse", "HEAD")
		if e != nil {
			return r, e
		}
		return r, &TargetMovedError{Expected: expectedBase, Actual: tip, Commit: c.SHA, NoWrite: true, OwnCommitKnown: head == c.SHA && objectID(expectedBase)}
	}
	if _, err = b.git(ctx, w, nil, "merge-base", "--is-ancestor", tip, c.SHA); err != nil {
		return r, fmt.Errorf("non-fast-forward integration refused")
	}
	if _, err = b.git(ctx, w, nil, "update-ref", ref, c.SHA, tip); err != nil {
		// Only Git's explicit expected-old-value mismatch proves no write.
		// Other failures (including cancellation) remain uncertain.
		if ctx.Err() == nil && strings.Contains(err.Error(), "is at ") && strings.Contains(err.Error(), " but expected ") {
			current, readErr := b.integrationTip(ctx, w, ref)
			if readErr != nil {
				return r, err
			}
			if _, ancestorErr := b.git(ctx, w, nil, "merge-base", "--is-ancestor", c.SHA, current); ancestorErr == nil {
				return IntegrationEvidence{c.SHA, current, true}, nil
			}
			contained, inspectErr := b.equivalentAt(ctx, w, c.SHA, current)
			if inspectErr == nil && contained {
				return IntegrationEvidence{c.SHA, current, true}, nil
			}
			if inspectErr == nil && current != tip {
				head, headErr := b.git(ctx, w, nil, "rev-parse", "HEAD")
				if headErr == nil {
					return r, &TargetMovedError{Expected: tip, Actual: current, Commit: c.SHA, NoWrite: true, OwnCommitKnown: head == c.SHA && objectID(expectedBase)}
				}
			}
		}
		return r, err
	}
	return IntegrationEvidence{SHA: c.SHA, Landed: true}, nil
}

// Reconcile currently only certifies an unchanged base; no lossy replay fallback.
func (b *Broker) Reconcile(ctx context.Context, w worktree.Binding, base string) error {
	if err := b.validate(ctx, w); err != nil {
		return err
	}
	head, err := b.git(ctx, w, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != base {
		return fmt.Errorf("base changed; reconciliation required")
	}
	return nil
}

// observedCompile requires both forced compilation and a compiler invocation
// naming source that actually exists inside this worktree. Linker activity or
// compiling only generated _testmain.go is insufficient.
func observedCompile(stdout, stderr, root string, argv []string) bool {
	force, trace := false, false
	for _, a := range argv {
		force = force || a == "-a"
		trace = trace || a == "-x"
	}
	if !force || !trace {
		return false
	}
	lines := strings.Split(stderr, "\n")
	for _, line := range strings.Split(stdout, "\n") {
		var e struct{ Action, Output string }
		if json.Unmarshal([]byte(line), &e) == nil && e.Action == "build-output" {
			lines = append(lines, strings.Split(e.Output, "\n")...)
		}
	}
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasSuffix(f[0], "/compile") {
			continue
		}
		for _, arg := range f[1:] {
			if !strings.HasSuffix(arg, ".go") {
				continue
			}
			p := arg
			if !filepath.IsAbs(p) {
				p = filepath.Join(root, p)
			}
			physical, err := filepath.EvalSymlinks(p)
			if err != nil {
				continue
			}
			rel, err := filepath.Rel(root, physical)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
				return true
			}
		}
	}
	return false
}

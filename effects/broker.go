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
	"github.com/xleelz/fae/proof"
	"github.com/xleelz/fae/provider"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

type Broker struct {
	configuredNested              bool
	Scratch                       []string
	Manager                       *worktree.Manager
	Allowed                       [][]string
	Environment                   []string
	MaxOutput                     int
	ProcessRegistry, ProcessOwner string
	ProcessSession                string
	// ProcessLimit caps every brokered run's tasks (FAE-HOST-015); nil runs
	// uncapped where the host cannot delegate pids.max.
	ProcessLimit *ProcessLimit
	// ScratchLeft, when set, is told of a run's scratch or policy directory
	// the broker could not remove (removeRunScratch); the daemon logs it,
	// and its next start's sweep removes or quarantines it.
	ScratchLeft func(dir string, err error)
	// NestedCommands is how a run's nested tool calls are handled
	// (FAE-HOST-014): refused ("" or NestedRefuse) or accounted.
	NestedCommands NestedMode
	// NestedBroker is the fae executable whose `heavy-op run --shim-policy`
	// applies account mode's nested-command policy
	// (commandshim.SelfBroker). Account mode refuses to run without it.
	NestedBroker string
	// RecordNested, when set, durably records an account-mode run's nested
	// calls before Run returns its evidence; a run it cannot record fails.
	// The context carries the run's NestedRun, when the caller set one.
	RecordNested func(context.Context, RunEvidence) error
	// CommitIdentity and ContentPolicy enforce FAE-VCS-012 and FAE-SEC-009.
	CommitIdentity *CommitIdentity
	ContentPolicy  ContentPolicy
}

// NestedMode selects the PATH wrappers a brokered command's nested go, gofmt,
// git and other build tool calls meet (FAE-HOST-014).
type NestedMode string

const (
	// NestedRefuse, the default, refuses every nested call with exit 77
	// (commandshim.InstallDenials), except a Local task's git push or remote
	// change, which exits 78 (commandshim.InstallLocalDenials, FAE-MODE-002).
	NestedRefuse NestedMode = "refuse"
	// NestedAccount records every nested call and passes it through the
	// nested-command policy (commandshim.PrepareAccounted: Go's parallelism
	// clamp, path, output and flag allowlists, formatter limits, and git that
	// may change only the run's scratch and temporary repositories, and
	// never pushes, changes a remote, reaches a repository off this host or
	// runs a program the call names), then runs the tool in the brokered
	// command's process group, task cap and lease window, under no lease of
	// its own (commandshim.InstallAccounting). That holds for the calls that
	// reach the wrappers: a program git starts, such as a filter a test's own
	// repository configures, runs the real git (no nested call runs a hook).
	NestedAccount NestedMode = "account"
)

// NestedRun identifies the task dispatch a brokered command runs for. The
// daemon sets it on the context of a dispatch's host commands (WithNestedRun).
// Both nested modes apply the task's mode to nested git calls: a Local task's
// push or remote change exits 78 (FAE-MODE-002), any other task's 77. A run
// whose context has none is treated as Local.
type NestedRun struct {
	TaskID, DispatchID string
	Local              bool
}

type nestedRunKey struct{}

// WithNestedRun returns ctx carrying run.
func WithNestedRun(ctx context.Context, run NestedRun) context.Context {
	return context.WithValue(ctx, nestedRunKey{}, run)
}

// NestedRunOf returns the NestedRun ctx carries, if any.
func NestedRunOf(ctx context.Context) (NestedRun, bool) {
	run, ok := ctx.Value(nestedRunKey{}).(NestedRun)
	return run, ok
}

// NestedSummaryLimit bounds the nested calls RunEvidence lists; NestedCount
// is exact.
const NestedSummaryLimit = 16

// DefaultMaxOutput is the output bound of a Broker without MaxOutput.
const DefaultMaxOutput = 4 << 20

type Command struct {
	OutputOwnershipRegistry  string
	TaskID                   string
	SharedWriteOwner         string
	ExcludedOutputDirs       []string
	SharedWriteRegistry      string
	RuntimeMountIdentities   map[string]string
	RuntimeMounts, CacheDirs []string
	Reads                    *workflow.ReadPolicy
	DenyPaths                []string
	ReadRootViews            map[string]provider.ReadRootView
	ReadRootPaths            []string `json:"read_root_paths,omitempty"`
	ArtifactsOnly            bool
	Generate                 *workflow.GenerateSpec
	Capture                  []string
	OutputSchema             json.RawMessage
	CaptureBytes             int
	Scratch                  []string
	EffectiveWriteScopes     [][]string
	ExecutionDir             string
	OutputDir                string
	StagingDir               string
	StagingWrite             bool
	Accept                   bool
	Argv                     []string
	Timeout                  time.Duration
	TestID, ExpectedFailure  string
	// Shape is a workflow-declared test shape. go-unit narrows the allowlisted
	// vector to exactly TestID; the allowlist still applies to Argv itself.
	Shape string
	// Environment is the stage's validated KEY=VALUE policy, set over the
	// sanitized base; nothing else is inherited from the daemon.
	Environment []string
	// Check configures structured check output parsing and allowed writes (FAE-WF-095).
	Check   *workflow.CheckSpec
	Formal  *proof.Config
	Adapter string
	// Writes lists path globs the check command is permitted to touch.
	Writes []string
	// DeclaredFiles lists the task's editable files/write_scope; Writes must be disjoint from it.
	DeclaredFiles []string
}
type RunEvidence struct {
	Proof                               *proof.Result     `json:"proof,omitempty"`
	ExecutableDigest                    string            `json:"executable_digest,omitempty"`
	RuntimeMounts                       map[string]string `json:"runtime_mounts,omitempty"`
	ViewConstructionMS, ViewCopiedBytes int64
	IsolationPolicy                     string
	CheckError                          string            `json:"check_error,omitempty"`
	Changed                             []string          `json:"changed,omitempty"`
	ChangedCount                        int               `json:"changed_count,omitempty"`
	Captured                            map[string]string `json:"captured,omitempty"`
	StagingDigest                       string            `json:"staging_digest,omitempty"`
	FreshBuild                          bool              `json:"fresh_build"`
	Stderr                              string            `json:"stderr"`
	ExitCode                            int               `json:"exit_code"`
	ExecutedCount                       int               `json:"executed_count"`
	MatchedCount                        int               `json:"matched_count"`
	ExpectedFailure                     bool              `json:"expected_failure"`
	RedPassed                           bool              `json:"red_passed"`
	OutputDigest                        string            `json:"output_digest"`
	Tree                                string            `json:"tree"`
	Output                              string            `json:"output"`
	Truncated                           bool              `json:"truncated"`
	Environment                         []string          `json:"environment"`
	ProcessLimit                        int               `json:"process_limit,omitempty"`
	CheckPayload                        map[string]any    `json:"check_payload,omitempty"`
	CheckCount                          float64           `json:"check_count,omitempty"`
	// NestedCount and Nested are an account-mode run's nested tool calls: the
	// exact count and the first NestedSummaryLimit in call order.
	NestedCount int                      `json:"nested_count,omitempty"`
	Nested      []commandshim.NestedCall `json:"nested,omitempty"`
	// Command and Started identify the run: the command vector as declared
	// and the time the broker started it.
	Command []string  `json:"command,omitempty"`
	Started time.Time `json:"started"`
	// TimedOut marks a run the broker killed at its deadline (Command.Timeout).
	// It is a failed run, not an error: its process group is retired, ExitCode
	// is -1 and the output it wrote is kept. It never proves an expected
	// failure, and in a go test a test it cut off is not counted as executed
	// (parseTests).
	TimedOut bool `json:"timed_out,omitempty"`
}

func (b *Broker) validate(ctx context.Context, w worktree.Binding) error {
	if b.Manager == nil {
		return fmt.Errorf("manager required")
	}
	return b.Manager.Verify(ctx, w)
}

// cleanEnv is the environment of a brokered command. tmp is its TMPDIR, on
// disk beside the worktree: the system temporary directory is treated as
// RAM-backed (FAE-HOST-020).
func cleanEnv(extra []string, tmp string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + tmp, "LANG=C", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	for _, e := range extra {
		key, _, ok := strings.Cut(e, "=")
		if ok && (key == "GOCACHE" || key == "GOMODCACHE" || key == "GOPATH" || key == "GOTOOLCHAIN" || key == "GOPROXY" || key == "GOSUMDB") {
			env = append(env, e)
		}
	}
	return env
}

// GoRootGap is commandshim.GoRootGap for the environment b's runs start from
// (before a stage's own): why they get no GOROOT mirror although they have a
// go, "" when they get one or have no go.
func (b *Broker) GoRootGap() (goBinary, gap string) {
	return commandshim.GoRootGap(cleanEnv(b.Environment, os.TempDir()))
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

// Run executes one allowlisted command in w. In account mode its nested calls
// are passed to RecordNested before the evidence is returned.
func (b *Broker) Run(ctx context.Context, w worktree.Binding, c Command) (RunEvidence, error) {
	r, err := b.run(ctx, w, c)
	if err == nil && b.NestedCommands == NestedAccount && b.RecordNested != nil {
		err = b.RecordNested(ctx, r)
	}
	return r, err
}

// installNested installs a run's PATH wrappers for nested tool calls: the
// refusals, or accounting wrappers whose policy scopes the calls to the
// worktree and the run's scratch directory and resolves real tools from env's
// PATH, go and gofmt first from goRoot's bin. mirror is the run's GOROOT
// mirror, "" for none. records is the accounting directory, "" when refusing.
func (b *Broker) installNested(ctx context.Context, root, worktree, scratch string, env []string, goRoot, mirror string) (shims, records string, err error) {
	run, ok := NestedRunOf(ctx)
	switch b.NestedCommands {
	case "", NestedRefuse:
		if !ok || run.Local {
			shims, err = commandshim.InstallLocalDenials(root)
		} else {
			shims, err = commandshim.InstallDenials(root)
		}
		return shims, "", err
	case NestedAccount:
		if b.NestedBroker == "" {
			return "", "", errors.New("nested_commands account needs the fae executable's heavy-op run to apply the nested-command policy; run the daemon as the fae binary")
		}
		search := commandshim.BasePath(env)
		if goRoot != "" {
			search = filepath.Join(goRoot, "bin") + string(os.PathListSeparator) + search
		}
		// Fae's own suite keeps fixtures in an on-disk /var/tmp and builds
		// there, so that root is in the nested calls' scope and not RAM-backed.
		temp, ram := commandshim.AccountedTemp()
		nested := &commandshim.NestedGo{GoRoot: mirror, Env: commandshim.NestedGoKeptEnv(env), Temp: temp, Programs: commandshim.NestedGoPrograms(env)}
		return commandshim.InstallAccounting(root, commandshim.RoleShims{Configured: b.configuredNested, Broker: b.NestedBroker, Worktree: worktree, Scratch: scratch, Local: !ok || run.Local, NestedGo: nested, RAMRoots: ram}, search)
	}
	return "", "", fmt.Errorf("unknown nested command mode %q", b.NestedCommands)
}

// RefusedBeforeExec is a brokered run that failed before its command's
// process existed: the worktree, command vector, timeout, shape or policy
// was refused, or the run's scratch, wrappers or executable could not be
// prepared. Nothing ran, so the caller may charge it as a refusal before
// launch (FAE-RT-004). A caller's cancellation is never wrapped.
type RefusedBeforeExec struct{ Cause error }

func (e *RefusedBeforeExec) Error() string { return "refused before exec: " + e.Cause.Error() }
func (e *RefusedBeforeExec) Unwrap() error { return e.Cause }

func (b *Broker) run(ctx context.Context, w worktree.Binding, c Command) (r RunEvidence, err error) {
	caller, started := ctx, false
	defer func() {
		if err != nil && !started && caller.Err() == nil {
			err = &RefusedBeforeExec{Cause: err}
		}
	}()
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
	forbidden := append([]string{w.Path, c.OutputDir, c.StagingDir, b.ProcessOwner}, c.ReadRootPaths...)
	if err = agentruntime.ValidateCacheDirectories(c.CacheDirs, append(append([]string{}, forbidden...), c.RuntimeMounts...)); err != nil {
		return r, err
	}
	r.RuntimeMounts, err = agentruntime.RuntimeMountIdentities(c.RuntimeMounts, forbidden)
	if err != nil {
		return r, err
	}
	for path, pinned := range c.RuntimeMountIdentities {
		if r.RuntimeMounts[path] != pinned {
			return r, fmt.Errorf("runtime mount identity changed: %s", path)
		}
	}
	if c.Adapter != "" && c.Adapter != "builtin" && c.Adapter != "configured" {
		return r, fmt.Errorf("unknown command adapter %q", c.Adapter)
	}
	languageShape := c.Shape == "go-unit" || c.Shape == "go-integration" || c.Shape == "rust-unit" || c.Shape == "python-pytest" || c.Shape == "node-test"
	configured := (c.Formal != nil || c.Adapter == "configured") && !languageShape
	if c.Formal != nil {
		if err = proof.Validate(*c.Formal); err != nil {
			return r, err
		}
		if c.Adapter == "builtin" || c.Check != nil || c.Generate != nil || c.Shape != "" || c.Accept {
			return r, fmt.Errorf("ambiguous formal command configuration")
		}
	}
	r.IsolationPolicy = "cli-and-collector"
	writes := c.Writes
	if c.Check != nil {
		if err := workflow.ValidateCheckSpec(*c.Check); err != nil {
			return r, err
		}
		if len(writes) == 0 {
			writes = c.Check.Writes
		}
	}
	if c.Generate != nil {
		writes = c.Generate.Writes
		for _, layer := range c.EffectiveWriteScopes {
			if !workflow.GlobScopeNarrows(writes, layer) {
				return r, fmt.Errorf("generator writes exceed effective write scope")
			}
		}
	} else if err := workflow.CheckWritesDisjoint(writes, c.DeclaredFiles); err != nil {
		return r, err
	}
	copied := *b
	copied.configuredNested = configured
	copied.Scratch = append(append([]string{}, b.Scratch...), c.Scratch...)
	if c.Generate == nil && c.Check != nil {
		copied.Scratch = append(copied.Scratch, writes...)
	}
	b = &copied
	shaped := c.Argv
	if c.Shape != "" {
		var err error
		if shaped, err = shapeVector(c.Argv, c.Shape, c.TestID); err != nil {
			return r, err
		}
		shaped = brokerGoTestAlarm(shaped, c.Shape, c.Timeout)
	}
	commandPolicy := commandshim.Policy{Worktree: w.Path, CWD: w.Path, CodeRole: true, LeaseHeld: true, Local: true, RAMRoots: []string{"/tmp", "/var/tmp", "/dev/shm"}}
	var argv []string
	if configured {
		argv, err = commandPolicy.PrepareConfigured(shaped)
	} else {
		argv, err = commandPolicy.Prepare(shaped)
	}
	if err != nil {
		return r, err
	}
	before, err := b.Tree(ctx, w)
	if err != nil {
		return r, err
	}
	sourceDigest := ""
	if c.Formal != nil {
		sourceDigest, err = formalSourceDigest(w.Path, before, c.Formal.Sources)
		if err != nil {
			return r, err
		}
	}
	parent := ctx
	ctx, cancel := context.WithTimeoutCause(ctx, c.Timeout, errBrokerRunDeadline)
	defer cancel()
	// Resolve before PATH wrappers are installed: only this admitted invocation bypasses them.
	binary, err := lookCommand(w.Path, argv[0])
	if err != nil {
		return r, err
	}
	if c.Formal != nil {
		r.ExecutableDigest, err = formalExecutableDigest(binary)
		if err != nil {
			return r, err
		}
	}
	shimRoot, err := os.MkdirTemp(filepath.Dir(w.Path), "command-policy-")
	if err != nil {
		return r, err
	}
	defer b.removeRunScratch(shimRoot)
	// Build work and test scratch directories live on disk and are removed on
	// every exit path (FAE-HOST-020), with any read-only tree a test left in
	// them (removeRunScratch).
	scratch, err := os.MkdirTemp(filepath.Dir(w.Path), "command-tmp-")
	if err != nil {
		return r, err
	}
	defer b.removeRunScratch(scratch)
	base := append(cleanEnv(b.Environment, scratch), c.Environment...)
	if configured {
		base, err = commandshim.ConfiguredEnvironment(argv, base, scratch)
	} else {
		base, err = commandshim.LanguageEnvironment(argv, base, scratch)
	}
	if err != nil {
		return r, err
	}
	pythonReport := ""
	if kind, supported := commandshim.PythonCommandKind(argv); c.Formal == nil && c.Check == nil && supported && kind == "test" {
		pythonReport = filepath.Join(scratch, "pytest-results.xml")
		argv, err = AddPythonJUnitReport(argv, pythonReport)
		if err != nil {
			return r, err
		}
	}
	// go test starts its tests with $GOROOT/bin first on PATH; under the
	// run's GOROOT mirror that is the wrappers (commandshim.InstallGoRoot).
	// The mirror is sealed read-only (not containment: the run may chmod it).
	// A go outside any GOROOT gets no mirror; the daemon logs that gap at
	// start (GoRootGap) and fae doctor reports it.
	goBinary := ""
	if filepath.Base(binary) == "go" {
		goBinary = binary
	}
	goRoot, mirror := commandshim.GoRoot(goBinary, base), ""
	if goRoot != "" {
		if mirror, err = commandshim.InstallGoRoot(shimRoot, goRoot); err != nil {
			return r, err
		}
		if err = commandshim.SealGoRoot(mirror); err != nil {
			return r, err
		}
	}
	shims, records, err := b.installNested(ctx, shimRoot, w.Path, scratch, base, goRoot, mirror)
	if err != nil {
		return r, err
	}
	run, env := append([]string{binary}, argv[1:]...), commandshim.GoRunEnv(commandshim.PrependPath(base, shims), mirror)
	if b.ProcessLimit != nil {
		run, env = b.ProcessLimit.wrap(run, env)
		r.ProcessLimit = b.ProcessLimit.Tasks
	}
	cmd := exec.CommandContext(ctx, run[0], run[1:]...)
	cmd.Dir = w.Path
	if c.ArtifactsOnly && c.OutputDir != "" {
		cmd.Dir = c.OutputDir
	}
	cmd.Env = env
	cacheKey := fmt.Sprintf("%x", sha256.Sum256([]byte(w.Path)))
	cacheMounts, cacheErr := agentruntime.PrivateCacheDirectories(filepath.Join(filepath.Dir(w.Path), "task-state", cacheKey), c.CacheDirs)
	if cacheErr != nil {
		return r, cacheErr
	}
	cmd.Env = agentruntime.ApplyPrivateCaches(cmd.Env, cacheMounts)
	var directoryView *provider.SessionView
	if len(c.ReadRootPaths) > 0 || c.OutputDir != "" || c.StagingDir != "" || c.Reads != nil || len(c.DenyPaths) > 0 {
		guard := &provider.ReadGuard{OutputOwnershipRegistry: c.OutputOwnershipRegistry, Command: true, WritableWorktree: !c.ArtifactsOnly && (len(writes) > 0 || len(c.Scratch) > 0 || c.Check == nil), RuntimeCacheDirs: cacheMounts, RuntimeReadDirs: append([]string{goRoot}, c.RuntimeMounts...), Policy: c.Reads, DenyPaths: c.DenyPaths, Worktree: w.Path, ReadRoots: map[string]provider.ReadRootView{}, WritableViews: map[string]func(string) bool{}, WritableFiles: []string{records}}
		guard.CanWrite = func(rel string) bool {
			if c.ArtifactsOnly {
				return false
			}
			if _, protected := workflow.ProtectedPath(rel); protected {
				return false
			}
			if workflow.MatchesAnyGlob(rel, c.Scratch) {
				return true
			}
			return workflow.MatchesAnyGlob(rel, writes)
		}
		if len(c.ReadRootViews) > 0 {
			for name, view := range c.ReadRootViews {
				guard.ReadRoots[name] = view
			}
		} else {
			for i, path := range c.ReadRootPaths {
				guard.ReadRoots[fmt.Sprint(i)] = provider.ReadRootView{Path: path}
			}
		}
		if c.OutputDir != "" {
			if c.ArtifactsOnly {
				guard.WritableViews[c.OutputDir] = func(rel string) bool {
					if allowed, e := provider.OutputWriteAllowed(c.OutputOwnershipRegistry, c.TaskID, filepath.Join(c.OutputDir, filepath.FromSlash(rel))); e != nil || !allowed {
						return false
					}
					for _, excluded := range c.ExcludedOutputDirs {
						relative, e := filepath.Rel(excluded, filepath.Join(c.OutputDir, filepath.FromSlash(rel)))
						if e == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
							return false
						}
					}
					allowed := c.Generate == nil || workflow.MatchesAnyGlob(rel, writes)
					return allowed && (len(c.EffectiveWriteScopes) == 0 || workflow.AllowsWriteScopes(rel, c.EffectiveWriteScopes))
				}
			} else {
				guard.ReadRoots["output"] = provider.ReadRootView{Path: c.OutputDir}
			}
		}
		if c.StagingDir != "" {
			if c.StagingWrite {
				guard.WritableViews[c.StagingDir] = func(string) bool { return true }
			} else {
				guard.ReadRoots["staging"] = provider.ReadRootView{Path: c.StagingDir}
			}
		}
		var viewErr error
		directoryView, viewErr = guard.ConfigureSessionView(cmd, shimRoot, []string{scratch, shimRoot, c.OutputDir, c.StagingDir})
		if directoryView != nil {
			defer directoryView.Close()
		}
		if viewErr != nil {
			return r, viewErr
		}
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = time.Second
	h := sha256.New()
	limit := b.MaxOutput
	if limit <= 0 {
		limit = DefaultMaxOutput
	}
	out := &bounded{digest: h, limit: limit}
	cmd.Stdout = out
	errHash := sha256.New()
	errOut := &bounded{digest: errHash, limit: limit}
	cmd.Stderr = errOut
	registry, owner := b.ProcessRegistry, b.ProcessOwner
	temporaryRegistry, retiredRegistry := false, false
	if registry == "" {
		registry, err = os.MkdirTemp(filepath.Dir(w.Path), "command-owner-")
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
	var nativeCollectors []*provider.NativeDirectoryCollector
	if directoryView == nil || !directoryView.Isolated() {
		for _, entry := range []struct {
			path  string
			allow func(string) bool
		}{
			{c.OutputDir, func(rel string) bool {
				for _, excluded := range c.ExcludedOutputDirs {
					relative, e := filepath.Rel(excluded, filepath.Join(c.OutputDir, filepath.FromSlash(rel)))
					if e == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
						return false
					}
				}
				return c.ArtifactsOnly && (c.Generate == nil || workflow.MatchesAnyGlob(rel, writes)) && (len(c.EffectiveWriteScopes) == 0 || workflow.AllowsWriteScopes(rel, c.EffectiveWriteScopes))
			}},
			{c.StagingDir, func(string) bool { return c.StagingWrite }},
		} {
			if entry.path == "" {
				continue
			}
			collector, e := provider.NewNativeDirectoryCollector(entry.path, shimRoot, entry.allow)
			if e != nil {
				return r, e
			}
			collector.SharedWrites(c.SharedWriteRegistry, c.SharedWriteOwner)
			collector.OutputOwnership(c.OutputOwnershipRegistry, c.TaskID)
			nativeCollectors = append(nativeCollectors, collector)
			defer collector.Close()
		}
	}
	var record *processowner.Record
	r.Command, r.Started = append([]string(nil), c.Argv...), time.Now().UTC()
	record, err = processowner.StartRole(cmd, registry, owner, b.ProcessSession, "host-command")
	var gateErr *processowner.GateError
	started = cmd.Process != nil && !errors.As(err, &gateErr)
	if err == nil {
		err = processowner.Wait(cmd, record)
		if retireErr := processowner.Retire(record); retireErr != nil {
			return r, fmt.Errorf("host process ownership unresolved in %s: %w", registry, retireErr)
		}
	}

	for _, collector := range nativeCollectors {
		if collectorErr := collector.Verify(); collectorErr != nil {
			return r, collectorErr
		}
	}
	if temporaryRegistry && record != nil {
		retiredRegistry = true
	}
	r.ExitCode = 0
	if err != nil {
		var exit *exec.ExitError
		switch {
		case asExit(err, &exit):
			r.ExitCode = exit.ExitCode()
		case ctx.Err() != nil && parent.Err() == nil && cmd.ProcessState != nil:
			// exec reports the deadline for a command that ended as the
			// deadline's kill ran; the command's own status stands.
			r.ExitCode = cmd.ProcessState.ExitCode()
		default:
			return r, err
		}
	}
	if ctx.Err() != nil {
		// A cancelled caller leaves the run's outcome unknown. The run's own
		// deadline does not: the command it killed is retired with its
		// process group, so the run failed by timing out (RunEvidence.TimedOut).
		if parent.Err() != nil || !errors.Is(context.Cause(ctx), errBrokerRunDeadline) {
			return r, ctx.Err()
		}
		r.TimedOut = cmd.ProcessState != nil && !cmd.ProcessState.Exited()
	}
	if records != "" {
		if r.NestedCount, r.Nested, err = commandshim.ReadAccounted(records, NestedSummaryLimit); err != nil {
			return r, err
		}
	}
	if viewErr := directoryView.Collect(); viewErr != nil {
		return r, viewErr
	}
	r.Output = string(out.data)
	r.Stderr = string(errOut.data)
	// Output past the bound is not kept (Truncated), but the command ended:
	// the run is evidence, not an error. Its exit code stands, and a go test
	// stream counts only the results its kept head reports (parseTests).
	r.Truncated = out.truncated || errOut.truncated || len(out.data)+len(errOut.data) > limit
	combined := sha256.Sum256(append(h.Sum(nil), errHash.Sum(nil)...))
	r.OutputDigest = hex.EncodeToString(combined[:])
	r.Tree = before
	r.Environment = cmd.Env
	var checkParseErr error
	if c.Check != nil {
		rawCheck := r.Output
		if c.Check.Output != "" && c.Check.Output != "stdout" {
			if !workflow.DeclaredPathSafe(c.Check.Output) {
				checkParseErr = fmt.Errorf("unsafe check output path %q", c.Check.Output)
			} else {
				outRoot := w.Path
				if c.ArtifactsOnly && c.OutputDir != "" {
					outRoot = c.OutputDir
				}
				outPath := filepath.Join(outRoot, filepath.FromSlash(c.Check.Output))
				data, readErr := os.ReadFile(outPath)
				if readErr != nil {
					checkParseErr = fmt.Errorf("read check output %q: %w", c.Check.Output, readErr)
				} else {
					rawCheck = string(data)
				}
			}
		}
		if checkParseErr == nil {
			r.CheckPayload, r.CheckCount, checkParseErr = workflow.ParseCheckOutput(*c.Check, rawCheck, r.ExitCode, r.TimedOut, r.Truncated, r.OutputDigest)
		}
	}
	if c.OutputDir != "" && c.ArtifactsOnly {
		changed, _, found := directoryView.DirectoryEvidence(c.OutputDir)
		if !found {
			for _, collector := range nativeCollectors {
				if ownChanges, _, ok := collector.DirectoryEvidence(c.OutputDir); ok {
					changed = ownChanges
					break
				}
			}
		}
		r.Changed = changed
		r.ChangedCount = len(changed)
	}
	if c.StagingDir != "" && c.Accept && r.ExitCode == 0 && !r.TimedOut && checkParseErr == nil {
		_, digest, found := directoryView.DirectoryEvidence(c.StagingDir)
		if !found {
			for _, collector := range nativeCollectors {
				if _, ownDigest, ok := collector.DirectoryEvidence(c.StagingDir); ok {
					digest = ownDigest
					break
				}
			}
		}
		r.StagingDigest = digest
		if r.CheckPayload != nil {
			r.CheckPayload["staging_digest"] = digest
		}
	}
	after, err := b.Tree(parent, w)
	if err != nil {
		return r, err
	}
	if after != before {
		changed, diffErr := b.changedPathsBetweenTrees(parent, w, before, after)
		if diffErr != nil {
			return r, diffErr
		}
		var offending []string
		for _, rel := range changed {
			permitted := workflow.MatchesAnyGlob(rel, writes) || workflow.MatchesAnyGlob(rel, c.Scratch)
			if c.Generate == nil && workflow.MatchesAnyGlob(rel, c.DeclaredFiles) {
				permitted = false
			}
			if c.Generate != nil && len(c.EffectiveWriteScopes) > 0 && !workflow.AllowsWriteScopes(rel, c.EffectiveWriteScopes) {
				permitted = false
			}
			if !permitted {
				offending = append(offending, rel)
			}
		}
		if len(offending) > 0 {
			if err := b.restorePathsToTree(parent, w, before, offending); err != nil {
				return r, err
			}
			return r, fmt.Errorf("host command mutated paths outside writes: %s", strings.Join(offending, ", "))
		}
		r.Changed = changed
		r.ChangedCount = len(changed)
		if c.Generate != nil {
			r.Tree = after
		}
	}
	if len(c.Capture) > 0 {
		limit := c.CaptureBytes
		if limit <= 0 {
			limit = 256 << 10
		}
		r.Captured = map[string]string{}
		for _, name := range c.Capture {
			text := r.Output
			if name == "stderr" {
				text = r.Stderr
			}
			if len(text) > limit {
				text = text[:limit]
				r.Truncated = true
			}
			r.Captured[name] = text
		}
		if r.CheckPayload != nil {
			for key, value := range r.Captured {
				r.CheckPayload[key] = value
			}
			r.CheckPayload["truncated"] = r.Truncated
		}
	}

	if checkParseErr == nil && c.Check != nil && len(c.OutputSchema) > 0 {
		raw, _ := json.Marshal(r.CheckPayload)
		checkParseErr = workflow.ValidateArtifact(c.OutputSchema, raw)
	}
	if checkParseErr != nil {
		r.CheckError = checkParseErr.Error()
		return r, nil
	}
	if c.Formal != nil {
		formalResult(c, &r, sourceDigest)
		afterDigest, sourceErr := formalSourceDigest(w.Path, after, c.Formal.Sources)
		if sourceErr != nil || sourceDigest != afterDigest {
			r.Proof.Outcome = proof.Error
			r.Proof.Passed = false
			r.Proof.Detail = "formal source changed during verification"
		}
		return r, nil
	}
	if c.Check == nil && len(c.Argv) > 1 && filepath.Base(c.Argv[0]) == "go" && c.Argv[1] == "test" {
		hasJSON, uncached := false, false
		for _, arg := range c.Argv[2:] {
			hasJSON = hasJSON || arg == "-json"
			uncached = uncached || arg == "-count=1"
		}
		if !hasJSON || !uncached {
			return r, fmt.Errorf("Go test evidence requires -json -count=1")
		}
		r.FreshBuild = observedCompile(r.Output, r.Stderr, w.Path, shaped)
		if err := parseTests(&r, c); err != nil {
			return r, err
		}
	}
	if c.Check == nil && pythonReport != "" {
		report, reportErr := readPythonReport(pythonReport)
		if reportErr != nil && !os.IsNotExist(reportErr) {
			return r, reportErr
		}
		parseErr := ParsePytestJUnit(&r, c, report)
		return r, parseErr
	}
	if c.Check == nil && len(c.Argv) > 1 && filepath.Base(c.Argv[0]) == "cargo" && c.Argv[1] == "test" {
		err := parseRustTests(&r, c)
		return r, err
	}
	if c.Check == nil && len(c.Argv) > 1 && filepath.Base(c.Argv[0]) == "node" && c.Argv[1] == "--test" {
		err := ParseNodeTestEvidence(&r, c)
		return r, err
	}
	if c.Check == nil && (c.Shape == "script" || c.Shape == "runner") {
		err := parseTAP(&r, c)
		if r.TimedOut || r.Truncated {
			// A TAP stream cut off at the deadline or the output bound counts
			// what it reported; the declared test proves nothing.
			r.ExpectedFailure, r.RedPassed, r.FreshBuild = false, false, true
			return r, nil
		}
		return r, err
	}
	return r, nil
}

// errBrokerRunDeadline is the cause of a brokered run's own deadline.
var errBrokerRunDeadline = errors.New("brokered run reached its deadline")

// brokerGoTestAlarm gives a shaped go test run go test's own -timeout of
// half the run's deadline, placed before its -run anchor, unless the vector
// already sets one. A test that hangs then fails within the run: go test
// prints "panic: test timed out after …" and the running test's name as that
// test's output, and the broker records the test as failed (parseTests),
// where the broker's deadline would kill go test with no result for the
// test at all. The other half of the deadline leaves room for the build,
// which go test's alarm does not count.
func brokerGoTestAlarm(argv []string, shape string, deadline time.Duration) []string {
	alarm := deadline / 2
	if shape != "go-unit" && shape != "go-integration" || alarm <= 0 || len(argv) < 3 || filepath.Base(argv[0]) != "go" || argv[1] != "test" {
		return argv
	}
	at := -1
	for i := 2; i < len(argv); i++ {
		a := argv[i]
		if a == "-timeout" || strings.HasPrefix(a, "-timeout=") || a == "-test.timeout" || strings.HasPrefix(a, "-test.timeout=") {
			return argv
		}
		if at < 0 && strings.HasPrefix(a, "-run=^") {
			at = i
		}
	}
	if at < 0 {
		return argv
	}
	return append(append(append([]string(nil), argv[:at]...), "-timeout="+alarm.String()), argv[at:]...)
}

// shapeArgv inserts an exact -run anchor for one Go test identifier before the
// package targets. Identifiers are restricted so the anchor is never a pattern.
func shapeArgv(argv []string, shape, test string) ([]string, error) {
	if shape != "go-unit" || len(argv) < 3 || filepath.Base(argv[0]) != "go" || argv[1] != "test" || !goTestName(test) {
		return nil, fmt.Errorf("unsupported declared test shape %q for %q", shape, test)
	}
	at := len(argv)
	for i := 2; i < len(argv); i++ {
		if argv[i] == "-run" || strings.HasPrefix(argv[i], "-run=") {
			return nil, fmt.Errorf("declared test shape conflicts with an explicit -run filter")
		}
		if argv[i] == "-p" || argv[i] == "-parallel" || argv[i] == "-test.parallel" || argv[i] == "-o" {
			i++ // separate flag value, not a package target
			continue
		}
		if !strings.HasPrefix(argv[i], "-") && at == len(argv) {
			at = i
		}
	}
	out := append(append(append([]string(nil), argv[:at]...), "-run=^"+test+"$"), argv[at:]...)
	return out, nil
}
func goTestName(s string) bool {
	if !strings.HasPrefix(s, "Test") {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}
func asExit(err error, p **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*p = e
	}
	return ok
}

// parseTests reads a go test -json stream: the tests executed (skipped ones
// excluded), those named c.TestID, and whether c.ExpectedFailure is in the
// failure text of that test's own output (redFailureText).
//
// A test counts as executed when it reported its own result, or when go
// test's own -timeout alarm named it (goTestAlarmNamed): the alarm ends the
// test binary, and go test (1.27) then reports only the package's fail. A
// test that started and has neither was cut off some other way while it ran:
// os.Exit or log.Fatal, a panic outside the test's goroutine, a TestMain
// goroutine, a signal from outside (an OOM kill prints "signal: killed"
// under it), or the broker itself, at the run's deadline (r.TimedOut) or its
// output bound (r.Truncated). It reported no failure of its own, so it is not
// counted as executed or matched and proves no expected failure, whatever was
// printed while it ran. The run is still evidence: its package's fail shows
// that go test ran on and ended on its own, and a run the broker cut is
// counted from what it kept, up to its last whole line. A test without a
// result in a package that passed, or in a package with no result in a
// stream the broker did not cut (the go process itself was killed), means
// the stream can't be read: "incomplete test stream", which leaves no
// evidence. A run killed at its deadline never proves its expected failure.
func parseTests(r *RunEvidence, c Command) error {
	output := r.Output
	cut := r.TimedOut || r.Truncated
	if cut {
		output = output[:strings.LastIndexByte(output, '\n')+1]
	}
	scan := bufio.NewScanner(strings.NewReader(output))
	scan.Buffer(make([]byte, 4096), 4<<20)
	type goTest struct{ pkg, name string }
	var tests []goTest
	started := map[goTest]bool{}
	ended := map[goTest]string{}
	packageEnded := map[string]string{}
	packageOutput := map[string]*strings.Builder{}
	var testOutput strings.Builder
	for scan.Scan() {
		var e struct{ Action, Package, Test, Output string }
		if err := json.Unmarshal(scan.Bytes(), &e); err != nil {
			return fmt.Errorf("malformed go test JSON: %w", err)
		}
		if e.Action == "output" {
			if packageOutput[e.Package] == nil {
				packageOutput[e.Package] = &strings.Builder{}
			}
			packageOutput[e.Package].WriteString(e.Output)
		}
		end := e.Action == "pass" || e.Action == "fail" || e.Action == "skip"
		if e.Test == "" {
			if end {
				packageEnded[e.Package] = e.Action
			}
			continue
		}
		k := goTest{e.Package, e.Test}
		if e.Action == "run" && !started[k] {
			started[k] = true
			tests = append(tests, k)
		}
		if end {
			ended[k] = e.Action
		}
		if e.Test == c.TestID {
			testOutput.WriteString(e.Output)
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	alarmNamed := map[string]map[string]bool{}
	for _, k := range tests {
		end := ended[k]
		if end == "" {
			if packageEnded[k.pkg] != "fail" && !cut {
				return fmt.Errorf("incomplete test stream")
			}
			if packageEnded[k.pkg] == "fail" && alarmNamed[k.pkg] == nil {
				alarmNamed[k.pkg] = goTestAlarmNamed(packageOutput[k.pkg].String())
			}
			if !alarmNamed[k.pkg][k.name] {
				continue // cut off: no result of its own
			}
			end = "fail"
		}
		if end == "skip" {
			continue
		}
		r.ExecutedCount++
		if c.TestID != "" && k.name == c.TestID {
			r.MatchedCount++
			r.RedPassed = end == "pass"
		}
	}
	r.ExpectedFailure = !r.TimedOut && c.ExpectedFailure != "" && r.ExitCode != 0 && r.MatchedCount == 1 && !r.RedPassed && strings.Contains(redFailureText(testOutput.String()), c.ExpectedFailure)
	return nil
}

// goTestAlarmNamed returns the tests go test's own -timeout alarm names in a
// package's output: its panic line "panic: test timed out after D" is
// followed by "running tests:" and one tab-indented "Name (D)" line for each
// test still running (Go 1.24 and later indent the panic's continuation
// lines with one more tab). A test the alarm names was running when the time
// ran out, which the RED gate can read as its failure (brokerGoTestAlarm).
func goTestAlarmNamed(output string) map[string]bool {
	named := map[string]bool{}
	lines := strings.Split(output, "\n")
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "panic: test timed out after ") || strings.TrimLeft(lines[i+1], "\t") != "running tests:" {
			continue
		}
		for i += 2; i < len(lines); i++ {
			entry := strings.TrimLeft(lines[i], "\t")
			name, rest, ok := strings.Cut(entry, " (")
			if entry == lines[i] || !ok || name == "" || !strings.HasSuffix(rest, ")") {
				break
			}
			named[name] = true
		}
	}
	return named
}

// redFailureText is the text a test's expected failure is matched against
// (FAE-WF-036, FAE-WF-073): the test's own output without the goroutine
// stacks in it and without go test's framing lines, which name the test and
// FAIL whatever the reason. A stack is left out from its "goroutine N
// [state]:" header through its frames (redStackLine): the dump a panic or a
// go test -timeout prints as the binary ends, and equally a stack printed
// while the test runs, which the test then continues past (net/http's log of
// a handler's recovered panic, debug.PrintStack), so the test's own later
// message still matches. What the test logs or prints itself, each logged
// line's file:line: prefix, a panic's message and signal line, and a
// timeout's "test timed out after" line with its list of running tests stay.
// A stack's frames and source paths name the test, its files, the module and
// the worktree whatever the reason, so no fragment matches them.
func redFailureText(output string) string {
	lines := strings.SplitAfter(output, "\n")
	var b strings.Builder
	for i := 0; i < len(lines); i++ {
		if redGoroutineDump(strings.TrimRight(lines[i], "\r\n")) {
			for i+1 < len(lines) && redStackLine(lines, i+1) {
				i++
			}
			continue
		}
		if !redFramingLine(lines[i]) {
			b.WriteString(lines[i])
		}
	}
	return b.String()
}

// redStackLine reports whether lines[i] continues a goroutine stack the Go
// runtime prints (runtime/traceback.go): a blank line between goroutines, a
// tab-indented source position, a call at column 0 followed by its position,
// "created by …", another goroutine's header, an elision marker
// ("...additional frames elided...", "...5 frames elided...") or an
// ancestor's "[originating from goroutine N]:". A test's own logged lines
// are indented with spaces and are none of these.
func redStackLine(lines []string, i int) bool {
	line := strings.TrimRight(lines[i], "\r\n")
	switch {
	case line == "", strings.HasPrefix(line, "\t"), strings.HasPrefix(line, "created by "), redGoroutineDump(line):
		return true
	case strings.HasPrefix(line, "...") && strings.HasSuffix(line, " elided..."):
		return true
	case strings.HasPrefix(line, "[originating from goroutine ") && strings.HasSuffix(line, "]:"):
		return true
	}
	return line[0] != ' ' && strings.Contains(line, "(") && strings.HasSuffix(line, ")") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "\t")
}

// redGoroutineDump reports the header line of a goroutine's stack as the Go
// runtime prints it, in the dump of a panic or a timeout and in
// debug.PrintStack: "goroutine 6 [running]:", "goroutine 1 [chan receive, 2
// minutes]:", with GOTRACEBACK=system's "gp=… m=…" fields before the bracket
// and GODEBUG=tracebacklabels=1's "{key: value}" labels after it.
func redGoroutineDump(line string) bool {
	rest, ok := strings.CutPrefix(line, "goroutine ")
	digits := len(rest) - len(strings.TrimLeft(rest, "0123456789"))
	return ok && digits > 0 && strings.HasPrefix(rest[digits:], " ") && strings.Contains(rest[digits:], "[") && (strings.HasSuffix(rest, "]:") || strings.Contains(rest, "] {") && strings.HasSuffix(rest, "}:"))
}

// redFramingLine reports a line go test prints around every test, whatever
// its reason for failing: === RUN, === PAUSE, === CONT, === NAME, --- FAIL:,
// --- PASS:, --- SKIP: and --- BENCH:, indented for a subtest.
func redFramingLine(line string) bool {
	line = strings.TrimLeft(line, " \t")
	for _, prefix := range []string{"=== RUN", "=== PAUSE", "=== CONT", "=== NAME", "--- FAIL:", "--- PASS:", "--- SKIP:", "--- BENCH:"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// literalPathGitEnv is the environment of every git call the broker makes on
// a worktree: the clean environment, the call's own settings, and last
// GIT_LITERAL_PATHSPECS=1, so a path after "--" is a file name and never a
// pathspec pattern or magic. The paths are a task's, chosen by a model: a file
// named ":(exclude)*" passed as a pathspec matched no file at all, and made
// Integrate's containment check report a commit the ref never received as
// landed (FAE-SEC-005).
func (b *Broker) literalPathGitEnv(w worktree.Binding, env []string) []string {
	return append(append(cleanEnv(b.Environment, filepath.Dir(w.Path)), env...), "GIT_LITERAL_PATHSPECS=1")
}

func (b *Broker) git(ctx context.Context, w worktree.Binding, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", w.Path}, args...)...)
	cmd.Env = b.literalPathGitEnv(w, env)
	var out []byte
	var err error
	if len(args) > 0 && args[0] == "update-ref" && b.ProcessRegistry != "" && b.ProcessSession != "" {
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		record, e := processowner.StartRole(cmd, b.ProcessRegistry, b.ProcessOwner, b.ProcessSession, "host-command")
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
	f, err := os.CreateTemp(filepath.Dir(w.Path), "command-index-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	defer os.Remove(name)
	defer os.Remove(name + ".lock")
	// A declared path is a file name, never a pathspec pattern or magic: a
	// path such as "*" or ":(top)" must not stage files outside the scope.
	// b.git passes every path literally (literalPathGitEnv).
	env := []string{"GIT_INDEX_FILE=" + name}
	if _, err = b.git(ctx, w, env, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	args := []string{"add", "--all", "--force", "--"}
	if files == nil {
		args = append(args, ".")
	} else {
		args = append(args, files...)
	}
	if _, err = b.git(ctx, w, env, args...); err != nil {
		return "", err
	}
	if len(b.Scratch) > 0 {
		names, err := b.git(ctx, w, env, "ls-files", "-z")
		if err != nil {
			return "", err
		}
		for _, rel := range strings.Split(names, "\x00") {
			if workflow.MatchesAnyGlob(rel, b.Scratch) {
				if _, err = b.git(ctx, w, env, "update-index", "--force-remove", "--", rel); err != nil {
					return "", err
				}
			}
		}
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

// IntegrationEvidence records a landing, or for a change already upstream the
// tip that contains it and the containment form (FAE-VCS-007).
type IntegrationEvidence struct {
	SHA, ContainedBy string
	Landed           bool
	Containment      string
}

// Containment forms.
const (
	ContainedAncestor = "ancestor"
	ContainedUpstream = "patch-or-blob-equivalent"
)

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
	finalMessage := b.CommitIdentity.FormatMessage(message)
	if !b.ContentPolicy.IsEmpty() {
		diff, err := b.git(ctx, w, nil, "diff", "--unified=0", s.Base, s.Tree)
		if err != nil {
			return r, err
		}
		if err := b.ContentPolicy.ScanCommit(finalMessage, s.Files, diff); err != nil {
			return r, err
		}
	}
	// Plumbing writes exactly the reviewed tree, never the mutable shared index.
	cmd := exec.CommandContext(ctx, "git", "-C", w.Path, "-c", "core.hooksPath=/dev/null", "commit-tree", s.Tree, "-p", s.Base)
	cmd.Env = b.literalPathGitEnv(w, b.CommitIdentity.GitEnv())
	cmd.Stdin = strings.NewReader(finalMessage + "\n")
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
		return IntegrationEvidence{c.SHA, tip, true, ContainedAncestor}, nil
	}
	contained, err := b.equivalentAt(ctx, w, c.SHA, tip)
	if err != nil {
		return r, err
	}
	if contained {
		return IntegrationEvidence{c.SHA, tip, true, ContainedUpstream}, nil
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
	if !b.ContentPolicy.IsEmpty() {
		if err := ScanGitRange(ctx, w.Path, tip, c.SHA, b.ContentPolicy); err != nil {
			return r, err
		}
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
				return IntegrationEvidence{c.SHA, current, true, ContainedAncestor}, nil
			}
			contained, inspectErr := b.equivalentAt(ctx, w, c.SHA, current)
			if inspectErr == nil && contained {
				return IntegrationEvidence{c.SHA, current, true, ContainedUpstream}, nil
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

func (b *Broker) changedPathsBetweenTrees(ctx context.Context, w worktree.Binding, before, after string) ([]string, error) {
	out, err := b.git(ctx, w, nil, "diff-tree", "-r", "--name-only", "--no-commit-id", "-z", before, after)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, filepath.ToSlash(p))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func (b *Broker) restorePathsToTree(ctx context.Context, w worktree.Binding, tree string, paths []string) error {
	for _, rel := range paths {
		if !workflow.DeclaredPathSafe(rel) {
			return fmt.Errorf("unsafe changed path %q", rel)
		}
		target := filepath.Join(w.Path, filepath.FromSlash(rel))
		if _, err := b.git(ctx, w, nil, "cat-file", "-e", tree+":"+rel); err == nil {
			cmd := exec.CommandContext(ctx, "git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", w.Path, "cat-file", "blob", tree+":"+rel)
			cmd.Env = b.literalPathGitEnv(w, nil)
			data, err := cmd.Output()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(target, data, 0644); err != nil {
				return err
			}
		} else {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			for dir := filepath.Dir(target); dir != w.Path && strings.HasPrefix(dir, w.Path+string(filepath.Separator)); dir = filepath.Dir(dir) {
				if err := os.Remove(dir); err != nil {
					break
				}
			}
		}
	}
	return nil
}

// BranchWorktree represents an isolated forked worktree for a change-producing
// FanOut branch (FAE-WF-096).
type BranchWorktree struct {
	Binding    worktree.Binding `json:"binding"`
	ParentTask string           `json:"parent_task"`
	FanStage   string           `json:"fan_stage"`
	BranchID   string           `json:"branch_id"`
	BaseCommit string           `json:"base_commit"`
	BaseTree   string           `json:"base_tree"`
	Ref        string           `json:"ref"`
	WriteScope []string         `json:"write_scope"`
	CommitSHA  string           `json:"commit_sha,omitempty"`
	Tree       string           `json:"tree,omitempty"`
	Files      []string         `json:"files,omitempty"`
}

// ForkBranchWorktree forks a per-branch git worktree from the parent task's
// HEAD for a change-producing FanOut branch (FAE-WF-096).
func (b *Broker) ForkBranchWorktree(ctx context.Context, parent worktree.Binding, fanStage, branchID string, writeScope []string) (BranchWorktree, error) {
	var bw BranchWorktree
	if err := b.validate(ctx, parent); err != nil {
		return bw, err
	}
	if fanStage == "" || branchID == "" {
		return bw, errors.New("fanStage and branchID are required")
	}
	if len(writeScope) == 0 {
		return bw, errors.New("change-producing fan-out branch requires write_scope")
	}
	for _, g := range writeScope {
		if !workflow.ValidWriteGlob(g) {
			return bw, fmt.Errorf("invalid branch write_scope glob %q", g)
		}
	}
	baseCommit, err := b.git(ctx, parent, nil, "rev-parse", "HEAD")
	if err != nil {
		return bw, err
	}
	baseTree, err := b.git(ctx, parent, nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return bw, err
	}
	parentWhole, err := b.Tree(ctx, parent)
	if err != nil {
		return bw, err
	}
	if parentWhole != baseTree {
		// If parent has uncommitted changes in its working tree, create a synthetic commit object for the fork base.
		cmd := exec.CommandContext(ctx, "git", "-C", parent.Path, "-c", "core.hooksPath=/dev/null", "commit-tree", parentWhole, "-p", baseCommit)
		cmd.Env = b.literalPathGitEnv(parent, b.CommitIdentity.GitEnv())
		cmd.Stdin = strings.NewReader("fanout base snapshot\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return bw, fmt.Errorf("commit-tree fanout base: %w: %s", err, out)
		}
		baseCommit = strings.TrimSpace(string(out))
		baseTree = parentWhole
	}
	runKey := parent.Branch + "/fan/" + fanStage
	binding, err := b.Manager.Ensure(ctx, runKey, branchID, baseCommit)
	if err != nil {
		return bw, err
	}
	head, err := b.git(ctx, binding, nil, "rev-parse", "HEAD")
	if err != nil {
		return bw, err
	}
	// Existing branch refs carry earlier turns and inner-join commits. Adopt
	// them unchanged; each fan visit uses a distinct caller-provided identity.
	_ = head
	return BranchWorktree{
		Binding:    binding,
		ParentTask: parent.Branch,
		FanStage:   fanStage,
		BranchID:   branchID,
		BaseCommit: baseCommit,
		BaseTree:   baseTree,
		Ref:        "refs/heads/" + binding.Branch,
		WriteScope: append([]string(nil), writeScope...),
		CommitSHA:  baseCommit,
		Tree:       baseTree,
	}, nil
}

// CommitBranchWorktree verifies that all changes in bw match bw.WriteScope
// (and declaredFiles when non-empty) and commits them on bw.Ref (FAE-WF-096).
func (b *Broker) CommitBranchWorktree(ctx context.Context, bw BranchWorktree, declaredFiles []string, message string) (BranchWorktree, error) {
	if err := b.validate(ctx, bw.Binding); err != nil {
		return bw, err
	}
	if len(bw.WriteScope) == 0 {
		return bw, errors.New("branch write_scope required")
	}
	tree, err := b.Tree(ctx, bw.Binding)
	if err != nil {
		return bw, err
	}
	if tree == bw.BaseTree {
		bw.Tree = tree
		bw.CommitSHA = bw.BaseCommit
		return bw, nil
	}
	changed, err := b.changedPathsBetweenTrees(ctx, bw.Binding, bw.BaseTree, tree)
	if err != nil {
		return bw, err
	}
	for _, rel := range changed {
		if !workflow.MatchesAnyGlob(rel, bw.WriteScope) {
			return bw, fmt.Errorf("fan-out branch %s modified %q outside branch write_scope %v", bw.BranchID, rel, bw.WriteScope)
		}
		if len(declaredFiles) > 0 && !workflow.MatchesAnyGlob(rel, declaredFiles) {
			return bw, fmt.Errorf("fan-out branch %s modified %q outside task declared files %v", bw.BranchID, rel, declaredFiles)
		}
	}
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("fanout %s branch %s", bw.FanStage, bw.BranchID)
	}
	snap, err := b.Snapshot(ctx, bw.Binding, changed)
	if err != nil {
		return bw, err
	}
	ev, err := b.Commit(ctx, bw.Binding, snap, message)
	if err != nil {
		return bw, err
	}
	bw.CommitSHA = ev.SHA
	bw.Tree = ev.Tree
	bw.Files = append([]string(nil), changed...)
	return bw, nil
}

// IntegrateBranchWorktrees integrates surviving change-producing FanOut
// branches into the parent task worktree in deterministic branch order (0..N-1)
// after verifying pairwise disjointness of their WriteScope globs (FAE-WF-096).
func (b *Broker) IntegrateBranchWorktrees(ctx context.Context, parent worktree.Binding, branches []BranchWorktree) (string, error) {
	if err := b.validate(ctx, parent); err != nil {
		return "", err
	}
	for i := 0; i < len(branches); i++ {
		for k := i + 1; k < len(branches); k++ {
			for _, g1 := range branches[i].WriteScope {
				for _, g2 := range branches[k].WriteScope {
					if workflow.GlobsOverlap(g1, g2) {
						return "", fmt.Errorf("fan-out branch %s write_scope %q overlaps branch %s write_scope %q", branches[k].BranchID, g2, branches[i].BranchID, g1)
					}
				}
			}
		}
	}
	for _, bw := range branches {
		if bw.BaseTree == "" && bw.BaseCommit != "" {
			if resolvedBaseTree, err := b.git(ctx, parent, nil, "rev-parse", bw.BaseCommit+"^{tree}"); err == nil {
				bw.BaseTree = resolvedBaseTree
			}
		}
		if (bw.Tree == "" || bw.Tree == bw.BaseTree) && bw.CommitSHA != "" && bw.CommitSHA != bw.BaseCommit {
			if resolvedTree, err := b.git(ctx, parent, nil, "rev-parse", bw.CommitSHA+"^{tree}"); err == nil {
				bw.Tree = resolvedTree
			}
		}
		if bw.CommitSHA == "" || bw.CommitSHA == bw.BaseCommit || bw.Tree == "" || bw.Tree == bw.BaseTree {
			continue
		}
		changed, err := b.changedPathsBetweenTrees(ctx, parent, bw.BaseTree, bw.Tree)
		if err != nil {
			return "", err
		}
		for _, rel := range changed {
			if !workflow.MatchesAnyGlob(rel, bw.WriteScope) {
				return "", fmt.Errorf("fan-out branch %s commit touched %q outside branch write_scope", bw.BranchID, rel)
			}
		}
		if err := b.restorePathsToTree(ctx, parent, bw.Tree, changed); err != nil {
			return "", err
		}
	}
	return b.Tree(ctx, parent)
}

// CleanupBranchWorktree removes a clean branch worktree created by ForkBranchWorktree.
func (b *Broker) CleanupBranchWorktree(ctx context.Context, bw BranchWorktree) error {
	if bw.ParentTask == "" || bw.FanStage == "" || bw.BranchID == "" {
		return nil
	}
	return b.Manager.Remove(ctx, bw.ParentTask+"/fan/"+bw.FanStage, bw.BranchID)
}

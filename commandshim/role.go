package commandshim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// ShimNames are the PATH-first wrappers every role session receives.
var ShimNames = []string{"go", "gofmt", "cargo", "rustc", "rustfmt", "make", "cmake", "ninja", "pytest", "ruff", "tsc", "git"}

// NestedMarker is set in a shim-brokered command's environment. It grants
// nothing: a nested shim call that sees it is refused (FAE-HOST-014), and one
// that strips it only queues for a lease of its own, never the parent's.
const NestedMarker = "FAE_SHIM_NESTED"

// RoleShims is one role session's shim policy (FAE-HOST-011). Build, Test and
// Format are the role's tool profile; pre-code roles have none of them.
type RoleShims struct {
	Configured         bool              `json:"configured,omitempty"`
	LeaseProxy         string            `json:"lease_proxy,omitempty"`
	ShellAllow         [][]string        `json:"shell_allow,omitempty"`
	DenyTestRuns       bool              `json:"deny_test_runs,omitempty"`
	DeniedTestCommands [][]string        `json:"denied_test_commands,omitempty"`
	ShellAuditPath     string            `json:"shell_audit_path,omitempty"`
	Broker             string            `json:"broker"`
	Ledger             string            `json:"ledger"`
	Worktree           string            `json:"worktree"`
	Local              bool              `json:"local"`
	Build              bool              `json:"build"`
	Test               bool              `json:"test"`
	Format             bool              `json:"format"`
	Tools              map[string]string `json:"tools"`
	RAMRoots           []string          `json:"ram_roots"`
	// NestedGo is the session's or run's Go setting (goroot.go): its GOROOT
	// mirror, whose bin/ holds the go and gofmt wrappers, and in account mode
	// the run's own Go build locations.
	NestedGo *NestedGo `json:"nested_go,omitempty"`
	// AlertSpool is the owning daemon's private host-alert spool (FAE-HOST-003).
	AlertSpool string `json:"alert_spool,omitempty"`
	// Shims, when set, are the only shims the role's profile allows (FAE-WF-042).
	Shims []string `json:"shims,omitempty"`
	// Accounted marks an account-mode engine run's nested-command policy
	// (InstallAccounting, FAE-HOST-014): RunShim applies PrepareAccounted and
	// execs the tool under the parent run's lease instead of taking a lease;
	// Ledger is unused. Scratch is the run's on-disk TMPDIR, the second root
	// of its scope.
	Accounted bool   `json:"accounted,omitempty"`
	Scratch   string `json:"scratch,omitempty"`
}

// DefaultRAMRoots are the temporary roots treated as RAM-backed (FAE-HOST-020).
var DefaultRAMRoots = []string{"/tmp", "/var/tmp", "/dev/shm"}

// SelfBroker is the running executable when it is the fae binary (which
// provides `heavy-op run`), else "" so wrappers fail closed (test binaries).
func SelfBroker() string {
	exe, err := os.Executable()
	if err != nil || filepath.Base(exe) != "fae" {
		return ""
	}
	return exe
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// InstallRoleShims installs wrappers that route heavy invocations through
// `fae heavy-op run --shim-policy` under the host heavy-op lease. Real tools are
// resolved now from searchPath, so a wrapper never finds itself. Without a
// broker or ledger the session gets the refusal wrappers (fail closed), whose
// git exits 78 for a Local session's push or remote change.
func InstallRoleShims(parent string, s RoleShims, searchPath string) (string, error) {
	if s.Broker == "" || s.Ledger == "" {
		if s.Local {
			return InstallLocalDenials(parent)
		}
		return InstallDenials(parent)
	}
	if !filepath.IsAbs(s.Broker) || !filepath.IsAbs(s.Ledger) || !filepath.IsAbs(s.Worktree) {
		return "", errors.New("role shims require an absolute broker, ledger and worktree")
	}
	dir := filepath.Join(parent, "command-shims")
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	s.Tools = map[string]string{}
	for _, name := range ShimNames {
		if p := resolveShimTool(name, dir, searchPath); p != "" {
			s.Tools[name] = p
		}
	}
	// A test the role's go test starts finds the session's wrappers first.
	if root := GoRoot(s.Tools["go"], nil); root != "" {
		mirror, err := InstallGoRoot(parent, root)
		if err != nil {
			return "", err
		}
		s.NestedGo = &NestedGo{GoRoot: mirror}
	}
	if s.RAMRoots == nil {
		s.RAMRoots = DefaultRAMRoots
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	policy := filepath.Join(dir, ".policy.json")
	if err = os.WriteFile(policy, raw, 0400); err != nil {
		return "", err
	}
	for _, name := range ShimNames {
		script := "#!/bin/sh\nexec " + shellQuote(s.Broker) + " heavy-op run --shim-policy " + shellQuote(policy) + " -- " + shellQuote(name) + " \"$@\"\n"
		if err = os.WriteFile(filepath.Join(dir, name), []byte(script), 0500); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// ReadRoleShims loads a policy written by InstallRoleShims.
func ReadRoleShims(path string) (RoleShims, error) {
	var s RoleShims
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	if !filepath.IsAbs(s.Worktree) || !s.Accounted && !filepath.IsAbs(s.Ledger) || s.Accounted && !filepath.IsAbs(s.Scratch) {
		return s, errors.New("invalid shim policy")
	}
	return s, nil
}

// shimCapability names the profile grant an invocation needs; "" is never allowed.
func shimCapability(s RoleShims, argv []string) (string, bool) {
	if kind, ok := LanguageCommandKind(argv); ok {
		switch kind {
		case "test":
			return kind, s.Test
		case "build":
			return kind, s.Build
		case "format", "formatter":
			return "formatter", s.Format
		}
	}
	switch filepath.Base(argv[0]) {
	case "go":
		if len(argv) > 1 && argv[1] == "test" {
			return "test", s.Test
		}
		return "build", s.Build
	case "gofmt":
		return "formatter", s.Format
	}
	return "", false
}

// gitValueOptions are git's global options that take their value as the next
// argument (git -C DIR, git -c NAME=VALUE, git --git-dir DIR, ...).
var gitValueOptions = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--super-prefix": true, "--config-env": true, "--attr-source": true}

// gitSubcommand is the subcommand of git's arguments: the first one that is
// neither a global option nor an option's value, so push in git -C dir push.
func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case gitValueOptions[args[i]]:
			i++
		case !strings.HasPrefix(args[i], "-"):
			return args[i]
		}
	}
	return ""
}

// ShimWeight is the Appendix A heavy-op weight of a prepared vector: a whole
// suite or a recursive package build is 2, a targeted compile or test 1.
func ShimWeight(argv []string) int {
	if kind, supported := LanguageCommandKind(argv); supported && kind == "test" {
		for _, arg := range argv[1:] {
			if arg == "--exact" || strings.HasPrefix(arg, "--test-name-pattern=") || strings.Contains(arg, "::") {
				return 1
			}
		}
		return 2
	}
	if len(argv) < 2 || filepath.Base(argv[0]) != "go" {
		return 1
	}
	for _, a := range argv[2:] {
		if a == "-p=1" || strings.HasSuffix(a, "/...") && argv[1] != "test" {
			return 2
		}
	}
	return 1
}

func shimChildEnv(env []string) []string {
	out := make([]string, 0, len(env)+3)
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		// GOFLAGS and a Go env file could reintroduce -toolexec or parallelism.
		if k != NestedMarker && k != "GOFLAGS" && k != "GOENV" {
			out = append(out, e)
		}
	}
	return append(out, NestedMarker+"=1", "GOFLAGS=", "GOENV=off")
}

// RunShim is the `heavy-op run --shim-policy` entry. h.Argv[0] names the shim;
// h.Env and h.Dir are the caller's. Policy refusals exit 77 (78 for a Local
// push or remote change) before any lease is requested; lease and host
// refusals exit 75; otherwise the command's own code.
// An accounted policy (an account-mode engine run's nested call) takes no
// lease: it applies PrepareAccounted and replaces this process with the tool,
// which keeps the call's process, process group and task cap; it returns only
// on a refusal or a failed exec.
// The caller reports a refusal's reason on standard error.
func RunShim(ctx context.Context, policyPath string, h HeavyOp) (int, error) {
	deny := func(e error) (int, error) {
		var r *Refusal
		if !errors.As(e, &r) {
			e = refuse(e.Error())
		}
		return Refused, e
	}
	s, err := ReadRoleShims(policyPath)
	if err != nil {
		return deny(fmt.Errorf("shim policy: %v", err))
	}
	if len(h.Argv) == 0 {
		return deny(refuse("empty command"))
	}
	name := filepath.Base(h.Argv[0])
	// FAE-MODE-002: a Local push or remote change exits with its own code,
	// whatever else would also refuse it (a nested call, the profile's shims).
	if sub := gitSubcommand(h.Argv[1:]); name == "git" && s.Local && gitExternalSubcommands[sub] {
		return LocalModeRefused, &LocalRefusal{"git " + sub + ": a Local task makes no external effect, so pushes and remote changes are refused"}
	}
	if h.Env == nil {
		h.Env = os.Environ()
	}
	if s.Accounted {
		if s.Tools[name] == "" {
			return deny(refuse(name + " is not on the session's base PATH"))
		}
		code, err := runAccounted(ctx, s, h)
		if code == Refused {
			return deny(err)
		}
		return code, err
	}
	for _, e := range h.Env {
		if strings.HasPrefix(e, NestedMarker+"=") {
			return deny(refuse("nested " + name + " inside a leased command is refused (FAE-HOST-014); run it as its own command"))
		}
	}
	if len(s.Shims) > 0 && !slices.Contains(s.Shims, name) {
		return deny(refuse(name + " is not an allowed command shim of this role's tool profile"))
	}
	switch name {
	case "git":
		return runRoleGit(ctx, s, h)
	case "go", "gofmt":
	default:
		if _, supported := LanguageCommandKind(h.Argv); !supported {
			return deny(refuse(name + " requires a scoped broker adapter (including version probes)"))
		}
	}
	capability, granted := shimCapability(s, h.Argv)
	if !granted {
		return deny(refuse("this role has no " + capability + " capability (pre-code roles never build, test or format)"))
	}
	real := s.Tools[name]
	if real == "" {
		return deny(refuse(name + " is not on the session's base PATH"))
	}
	dir := h.Dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return 126, err
		}
	}
	argv := append([]string{real}, h.Argv[1:]...)
	p := Policy{Worktree: s.Worktree, CWD: dir, CodeRole: granted, LeaseHeld: true, Local: s.Local, RAMRoots: s.RAMRoots}
	// Validate before queueing so a refusal never waits for the lease; Admit
	// re-measures paths at time of use, under the granted lease.
	prepared, err := p.Prepare(argv)
	if err != nil {
		return deny(err)
	}
	h.Argv, h.Weight, h.Class, h.Ledger, h.Dir, h.AlertSpool = prepared, ShimWeight(prepared), "role", s.Ledger, dir, s.AlertSpool
	h.proxySocket = s.LeaseProxy
	languageScratch, scratchErr := os.MkdirTemp(filepath.Dir(policyPath), "language-tmp-")
	if scratchErr != nil {
		return deny(scratchErr)
	}
	defer os.RemoveAll(languageScratch)
	h.Env, err = LanguageEnvironmentWithTools(argv, shimChildEnv(h.Env), languageScratch, s.Tools)
	if err != nil {
		return deny(err)
	}
	h.Env = GoRunEnv(h.Env, s.NestedGo.goRootMirror())
	h.Admit = func(leased bool) ([]string, error) {
		p.LeaseHeld = leased
		return p.Prepare(argv)
	}
	code, err := RunHeavyOp(ctx, h)
	var r *Refusal
	if errors.As(err, &r) {
		return deny(err)
	}
	return code, err
}

// runAccounted is RunShim for an accounted policy: the nested call passes the
// same shim (PrepareAccounted) and runs in this process's place, inside the
// parent run's process group, task cap and lease window, under no lease of its
// own. The caller's environment is kept: GOFLAGS was checked, not cleared, so
// a stage's GOFLAGS still reaches nested builds, and the clamp's explicit
// flags override any parallelism it sets. A go call gets the run's Go
// setting (accountedGoEnv) and a git call accountedGitExecEnv: no global or
// system configuration it does not name, local repositories only, no hook,
// file-system monitor, trace target from a file or bundle URIs; a git call
// on a repository execs pinned to the repository the policy checked. It
// returns only when the call is
// refused (77, or 78 for a Local push or remote change), cancelled before the
// exec, or the exec fails (126).
func runAccounted(ctx context.Context, s RoleShims, h HeavyOp) (int, error) {
	name := filepath.Base(h.Argv[0])
	dir := h.Dir
	var err error
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return 126, fmt.Errorf("nested %s: %w", name, err)
		}
	} else if dir, err = filepath.Abs(dir); err != nil {
		return 126, fmt.Errorf("nested %s: %w", name, err)
	}
	p := Policy{Worktree: s.Worktree, Scratch: s.Scratch, CWD: dir, Local: s.Local, RAMRoots: s.RAMRoots, Temp: s.NestedGo.tempRoots()}
	var argv []string
	if s.Configured && name != "git" {
		p.CodeRole, p.LeaseHeld = true, true
		argv, err = p.PrepareConfigured(append([]string{s.Tools[name]}, h.Argv[1:]...))
	} else {
		argv, err = p.PrepareAccounted(append([]string{s.Tools[name]}, h.Argv[1:]...), h.Env)
	}
	var local *LocalRefusal
	if errors.As(err, &local) {
		return LocalModeRefused, err
	}
	if err != nil {
		return Refused, err
	}
	if name == "go" && !s.Configured {
		if h.Env, err = p.accountedGoEnv(argv, h.Env, s.NestedGo); err != nil {
			return Refused, err
		}
	}
	if name == "git" {
		h.Env = accountedGitExecEnv(h.Env)
	}
	if _, supported := LanguageCommandKind(argv); supported && !s.Configured {
		var nestedScratch string
		nestedScratch, err = os.MkdirTemp(s.Scratch, "nested-language-")
		if err != nil {
			return Refused, err
		}
		// The owning broker removes Scratch after the entire process group
		// retires. These files must survive syscall.Exec below.
		h.Env, err = LanguageEnvironmentWithTools(argv, h.Env, nestedScratch, s.Tools)
		if err != nil {
			return Refused, err
		}
	}
	if err = ctx.Err(); err != nil {
		return 1, fmt.Errorf("nested %s not run: %w", name, err)
	}
	if h.Dir != "" {
		if err = os.Chdir(dir); err != nil {
			return 126, fmt.Errorf("nested %s: %w", name, err)
		}
	}
	err = syscall.Exec(argv[0], argv, h.Env)
	return 126, fmt.Errorf("nested %s: %w", name, err)
}

package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/scratchfs"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/workflow"
)

// Direct runs a trusted fae-agent-v1 bridge, not an arbitrary agent CLI. The
// bridge MUST enforce Config.Profile using its provider's actual tool controls.
// Merely putting the profile into a prompt does not implement this protocol.
type Direct struct {
	Command           []string
	Root              string
	Environment       map[string]string
	Timeout           time.Duration
	StartTimeout      time.Duration
	StallTimeout      time.Duration
	StallPollInterval time.Duration
	MaxHandoffBytes   int64
	TailBytes         int
	// Shims names the heavy-op broker and host ledger for role command shims
	// (FAE-HOST-011); per-launch fields come from the Launch. Empty refuses.
	Shims               commandshim.RoleShims
	DisablePreToolHook  bool
	OmitRoleShimsOnPath bool
	mu                  sync.Mutex
	live                map[string]*liveSession
	seen                map[string]bool
	prepareProcess      func(context.Context, *exec.Cmd, string) (*processHooks, error)
}
type Launch struct {
	OutputOwnershipRegistry       string                   `json:"output_ownership_registry,omitempty"`
	ExcludedOutputDirs            []string                 `json:"excluded_output_dirs,omitempty"`
	SharedWriteRegistry           string                   `json:"shared_write_registry,omitempty"`
	RuntimeMountIdentities        map[string]string        `json:"runtime_mount_identities,omitempty"`
	CacheMounts                   map[string]string        `json:"cache_mounts,omitempty"`
	BranchInstance                string                   `json:"branch_instance,omitempty"`
	RuntimeMounts                 []string                 `json:"runtime_mounts,omitempty"`
	CacheDirs                     []string                 `json:"cache_dirs,omitempty"`
	ShellRuntimeMountsWithoutView bool                     `json:"shell_runtime_mounts_without_view,omitempty"`
	IsolationRequired             bool                     `json:"isolation_required,omitempty"`
	Reads                         *workflow.ReadPolicy     `json:"reads,omitempty"`
	DenyPaths                     []string                 `json:"deny_paths,omitempty"`
	ReadRootPaths                 []string                 `json:"read_root_paths"`
	NativeReadDirs                []string                 `json:"native_read_dirs,omitempty"`
	OnToolEvent                   func(domain.ToolEvent)   `json:"-"`
	OutputDir                     string                   `json:"output_dir,omitempty"`
	StagingDir                    string                   `json:"staging_dir,omitempty"`
	StagingWrite                  bool                     `json:"staging_write,omitempty"`
	Scratch                       []string                 `json:"scratch,omitempty"`
	WriteScopes                   [][]string               `json:"write_scopes,omitempty"`
	DeniedTestCommands            [][]string               `json:"denied_test_commands,omitempty"`
	ArtifactsOnly                 bool                     `json:"artifacts_only,omitempty"`
	WorkflowID                    string                   `json:"workflow_id,omitempty"`
	WorkflowRevision              string                   `json:"workflow_revision,omitempty"`
	SessionPolicy                 string                   `json:"session_policy,omitempty"`
	FreshContext                  bool                     `json:"fresh_context,omitempty"`
	FreshRole                     bool                     `json:"fresh_role,omitempty"`
	WallSeconds                   int                      `json:"wall_seconds,omitempty"`
	MaxTurns                      int                      `json:"max_turns,omitempty"`
	DispatchID                    string                   `json:"dispatch_id"`
	SessionID                     string                   `json:"session_id"`
	TaskID                        string                   `json:"task_id"`
	Stage                         string                   `json:"stage,omitempty"`
	Visit                         int                      `json:"visit,omitempty"`
	Attempt                       int                      `json:"attempt,omitempty"`
	Role                          string                   `json:"role"`
	Worktree                      string                   `json:"worktree"`
	Mode                          string                   `json:"mode"`
	Session                       string                   `json:"session,omitempty"`
	ChangeProducer                bool                     `json:"change_producer,omitempty"`
	TurnBudget                    *workflow.TurnBudgetSpec `json:"turn_budget,omitempty"`
	Profile                       workflow.Profile         `json:"profile"`
	Binding                       workflow.Binding         `json:"binding"`
	Brief                         json.RawMessage          `json:"brief"`
	ReadOnlyFiles                 []string                 `json:"read_only_files,omitempty"`
	InlineFileBytes               int                      `json:"inline_file_bytes,omitempty"`
	InlineTurnBytes               int                      `json:"inline_turn_bytes,omitempty"`
	CheckpointIntervalSec         int                      `json:"checkpoint_interval_sec,omitempty"`
	ResumeSession                 string                   `json:"resume_session,omitempty"`
	ResumeTree                    string                   `json:"resume_tree,omitempty"`
	ClaudeSettings                string                   `json:"claude_settings,omitempty"`
	InstanceRoot                  string                   `json:"instance_root,omitempty"`
	OnCheckpoint                  func(TurnCheckpoint)     `json:"-"`
}
type BridgeConfig struct {
	Protocol string `json:"protocol"`
	Launch
	Handoff      string `json:"handoff"`
	LogDirectory string `json:"log_directory"`
}
type Handoff struct {
	DispatchID            string          `json:"dispatch_id"`
	SessionID             string          `json:"session_id"`
	TaskID                string          `json:"task_id"`
	Output                json.RawMessage `json:"output"`
	AssistantTurns        int             `json:"assistant_turns,omitempty"`
	ProviderSession       string          `json:"provider_session,omitempty"`
	CheckpointTree        string          `json:"checkpoint_tree,omitempty"`
	ResumedFromCheckpoint bool            `json:"resumed_from_checkpoint,omitempty"`
}
type Collected struct {
	RuntimeMounts                       map[string]string
	ViewConstructionMS, ViewCopiedBytes int64
	IsolationPolicy                     string
	ProviderStats                       *domain.ProviderSessionStats
	// NonExecution is positive transport proof that no provider code was released.
	NonExecution          bool
	ProcessesRetired      bool
	Output                json.RawMessage
	Tail, LogDirectory    string
	ProducedAt            time.Time
	AssistantTurns        int
	ProviderSession       string
	CheckpointTree        string
	ResumedFromCheckpoint bool
}
type TurnError struct {
	Class, Path, Role, DispatchID string
	Cause                         error
}

// Typed bridge refusals (fae-agent-v1). A trusted bridge that refuses its turn
// for a reason it can name writes BridgeRefusalFile in its log directory and
// exits with BridgeRefusalExit. Run then reports a TurnError of the recorded
// class whose Cause is the *BridgeRefusal. Any other nonzero exit, or either
// signal without the other, stays process-failed.
const (
	// RefusedOutput: the provider's turn completed and the bridge refused its
	// answer (schema, edit shape or edit rules) before writing any edit.
	RefusedOutput = "output-refused"
	// RefusedBeforeProvider: the bridge refused the turn (task scope, brief,
	// role policy or a local failure) before launching the provider.
	RefusedBeforeProvider = "refused-before-provider"
	// ProviderNotStarted: the provider never acknowledged the prompt: its
	// executable did not start, or it wrote nothing to its event stream and
	// left no answer, whether it exited on its own, the bridge's start bound
	// (Appendix A's 180 s agent start) stopped it with its processes, or the
	// bridge's timeout did. Nothing the model could do ran, so the turn is
	// relaunched within the agent-start bound, and an exhausted bound
	// charges the binding Unavailable (FAE-RT-004).
	ProviderNotStarted = "provider-not-started"
	// ProviderFailed: the provider acknowledged the prompt and then ended
	// without an answer: it exited with a failure on its own, or the
	// bridge's own timeout stopped it after it wrote something. The bridge
	// wrote nothing to the task (it writes edits only from a completed,
	// validated answer), and the provider ran with no tool that writes
	// (Codex's read-only sandbox, Claude Code with no tools, whose announced
	// surface the bridge checked). The executor charges the provider's
	// health and, with the tree unchanged and the processes retired, fails
	// the turn as malformed output: a bounded stage retry (FAE-WF-034).
	ProviderFailed = "provider-failed"
	// RefusedBinding: the bridge refused the turn's binding before launching
	// the provider: this bridge does not serve the binding's provider, or
	// the binding or the bridge's own configuration (model, effort,
	// credentials, executable, timeout, start bound) cannot run a turn.
	// Retrying the same binding cannot succeed until that configuration
	// changes, so the engine moves the turn to the next route (FAE-RT-010,
	// FAE-WF-082).
	RefusedBinding    = "binding-refused"
	BridgeRefusalFile = "bridge-refusal.json"
	// LaunchRefused is the runtime's own refusal of a launch before the
	// bridge started: an incomplete identity, a non-canonical worktree or
	// root, a bridge argument vector or brief the protocol refuses, or a
	// failed session directory, configuration, environment, role shim, brief
	// file or pipe. No process started (Collected.NonExecution), and no
	// provider is to blame: the executor types it refused before launch,
	// charging neither the attempt nor the binding (FAE-RT-004, FAE-RT-009).
	LaunchRefused = "launch-refused"
	// BridgeRefusalExit is sysexits' EX_DATAERR. No other exit of a
	// supervised bridge uses it: Go's own failures exit 1 or 2, and a signal
	// shows as 128+n in the pane and resident wrappers.
	BridgeRefusalExit = 65
)

// maxBridgeRefusalDetail bounds a recorded refusal's detail (bytes before
// JSON escaping); maxBridgeRefusalBytes bounds the record file.
const (
	maxBridgeRefusalDetail = 4 << 10
	maxBridgeRefusalBytes  = 64 << 10
)

// BridgeRefusal is a refusal the trusted bridge recorded for this turn.
type BridgeRefusal struct {
	Class, Detail string
}

func (e *BridgeRefusal) Error() string {
	switch e.Class {
	case RefusedOutput:
		return "bridge refused the provider's output: " + e.Detail
	case RefusedBinding:
		return "bridge refused the binding before the provider ran: " + e.Detail
	case ProviderNotStarted:
		return "the provider never acknowledged the prompt: " + e.Detail
	case ProviderFailed:
		return "the provider failed without an answer, and the bridge wrote nothing: " + e.Detail
	case EditsNotApplied:
		return "bridge could not apply the provider's edits and restored the worktree: " + e.Detail
	case TurnBudgetExhausted:
		return "agentic turn exhausted its turn_budget: " + e.Detail
	}
	return "bridge refused the turn before the provider ran: " + e.Detail
}

// BeforeProvider reports a refusal the bridge made before it launched the
// provider: no provider health is observed for it (FAE-RT-009). A provider
// that never acknowledged the prompt (ProviderNotStarted) was launched: no
// single start is charged, but an exhausted agent-start bound charges its
// binding Unavailable (FAE-RT-004), so it is not before the provider.
func (e *BridgeRefusal) BeforeProvider() bool {
	return e.Class == RefusedBeforeProvider || e.Class == RefusedBinding
}

// bridgeRefusalClassKnown reports a class a bridge may record.
func bridgeRefusalClassKnown(class string) bool {
	return class == RefusedOutput || class == RefusedBeforeProvider || class == RefusedBinding || class == ProviderNotStarted || class == ProviderFailed || class == EditsNotApplied
}

type bridgeRefusalRecord struct {
	Protocol   string `json:"protocol"`
	DispatchID string `json:"dispatch_id"`
	SessionID  string `json:"session_id"`
	TaskID     string `json:"task_id"`
	Class      string `json:"class"`
	Detail     string `json:"detail"`
}

// RecordBridgeRefusal writes the turn's refusal record in its log directory.
// The bridge then exits with BridgeRefusalExit. The record is created
// exclusively, so a turn has at most one.
func RecordBridgeRefusal(c BridgeConfig, class, detail string) error {
	if !bridgeRefusalClassKnown(class) && !IsTurnBudgetExhaustedClass(class) {
		return fmt.Errorf("unknown bridge refusal class %q", class)
	}
	if !filepath.IsAbs(c.LogDirectory) || c.DispatchID == "" || c.SessionID == "" || c.TaskID == "" {
		return errors.New("bridge refusal needs the turn's identity and absolute log directory")
	}
	if len(detail) > maxBridgeRefusalDetail {
		detail = strings.ToValidUTF8(detail[:maxBridgeRefusalDetail], "")
	}
	if detail == "" {
		detail = "unspecified"
	}
	raw, e := json.Marshal(bridgeRefusalRecord{Protocol: "fae-agent-v1", DispatchID: c.DispatchID, SessionID: c.SessionID, TaskID: c.TaskID, Class: class, Detail: detail})
	if e != nil {
		return e
	}
	f, e := os.OpenFile(filepath.Join(c.LogDirectory, BridgeRefusalFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(raw)
	return errors.Join(e, f.Close())
}

// collectBridgeRefusal returns the turn's typed refusal when the bridge exited
// with BridgeRefusalExit, published no handoff, and left one fresh, bounded,
// regular record naming this turn and a known class; otherwise nil.
func collectBridgeRefusal(exit error, dir, handoff string, l Launch, startedAt time.Time) *BridgeRefusal {
	var status interface{ ExitCode() int }
	if !errors.As(exit, &status) || status.ExitCode() != BridgeRefusalExit {
		return nil
	}
	if _, e := os.Lstat(handoff); !os.IsNotExist(e) {
		return nil
	}
	f, e := os.OpenFile(filepath.Join(dir, BridgeRefusalFile), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > maxBridgeRefusalBytes || st.ModTime().Before(startedAt) {
		return nil
	}
	raw, e := io.ReadAll(io.LimitReader(f, maxBridgeRefusalBytes+1))
	if e != nil || len(raw) > maxBridgeRefusalBytes {
		return nil
	}
	if raw, e = domain.CanonicalJSON(raw); e != nil {
		return nil
	}
	var r bridgeRefusalRecord
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&r) != nil || r.Protocol != "fae-agent-v1" || r.DispatchID != l.DispatchID || r.SessionID != l.SessionID || r.TaskID != l.TaskID || r.Detail == "" || len(r.Detail) > maxBridgeRefusalDetail {
		return nil
	}
	if !bridgeRefusalClassKnown(r.Class) && !IsTurnBudgetExhaustedClass(r.Class) {
		return nil
	}
	return &BridgeRefusal{Class: r.Class, Detail: r.Detail}
}

func (e *TurnError) Error() string {
	return fmt.Sprintf("%s: role=%s dispatch=%s handoff=%s: %v", e.Class, e.Role, e.DispatchID, e.Path, e.Cause)
}
func (e *TurnError) Unwrap() error { return e.Cause }

// Sessions returns a copy of only processes launched by this adapter.
func (d *Direct) Sessions() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]int{}
	for k, v := range d.live {
		out[k] = v.pid
	}
	return out
}

// Live lists this adapter's live sessions with their owner (FAE-RT-001).
func (d *Direct) Live() []LiveSession {
	d.mu.Lock()
	defer d.mu.Unlock()
	return listLive(d.live, d.Root)
}

// Retire stops one live session and waits for its whole process tree to be
// retired by its Run (FAE-RT-001).
func (d *Direct) Retire(ctx context.Context, session string) error {
	d.mu.Lock()
	s := d.live[session]
	d.mu.Unlock()
	return retireLive(ctx, s)
}

// sessionRole is the inventory role of a launch's direct child (FAE-HOST-021).
func sessionRole(hooks *processHooks) string {
	if hooks != nil {
		return "multiplexer-server"
	}
	return "agent-session"
}
func canonicalDirectory(p string) error {
	if !filepath.IsAbs(p) {
		return errors.New("directory must be absolute")
	}
	actual, e := filepath.EvalSymlinks(p)
	if e != nil {
		return e
	}
	if actual != filepath.Clean(p) {
		return errors.New("directory must be canonical, without symlinks")
	}
	st, e := os.Stat(p)
	if e != nil {
		return e
	}
	if !st.IsDir() {
		return errors.New("not a directory")
	}
	return nil
}
func (d *Direct) Run(ctx context.Context, l Launch) (result Collected, err error) {
	groupRetired, hooksRetired := true, true
	defer func() { result.ProcessesRetired = groupRetired && hooksRetired }()
	fail := func(class, path string, e error) (Collected, error) {
		if class == LaunchRefused {
			// Every launch refusal precedes the bridge's start: nothing ran.
			result.NonExecution = true
		}
		return result, &TurnError{class, path, l.Role, l.DispatchID, e}
	}
	if l.DispatchID == "" || l.SessionID == "" || l.TaskID == "" || l.Role == "" || (l.Mode != "local" && l.Mode != "normal") {
		return fail(LaunchRefused, "", errors.New("complete identity and mode required"))
	}
	if e := canonicalDirectory(l.Worktree); e != nil {
		return fail(LaunchRefused, "", e)
	}
	if e := canonicalDirectory(d.Root); e != nil {
		return fail(LaunchRefused, "", e)
	}
	if len(d.Command) == 0 || !filepath.IsAbs(d.Command[0]) {
		return fail(LaunchRefused, "", errors.New("bridge executable must be absolute"))
	}
	found := false
	for _, a := range d.Command {
		if a == "{config}" {
			found = true
		}
		if strings.ContainsRune(a, 0) {
			return fail(LaunchRefused, "", errors.New("NUL argument"))
		}
	}
	if !found {
		return fail(LaunchRefused, "", errors.New("fae-agent-v1 requires exact {config} argument"))
	}
	if !json.Valid(l.Brief) {
		return fail(LaunchRefused, "", errors.New("brief must be JSON"))
	}
	d.mu.Lock()
	if d.seen == nil {
		d.seen = map[string]bool{}
		d.live = map[string]*liveSession{}
	}
	if d.seen[l.SessionID] {
		d.mu.Unlock()
		return fail("uncertain-dispatch", "", errors.New("session already launched"))
	}
	d.seen[l.SessionID] = true
	d.mu.Unlock()
	if e := scratchfs.ReapSessionCopies(d.Root); e != nil {
		return fail(LaunchRefused, "", e)
	}
	defer func() {
		if groupRetired {
			err = errors.Join(err, scratchfs.ReapSessionCopies(d.Root))
		}
	}()
	startedAt := time.Now()
	dir, e := os.MkdirTemp(d.Root, "session-")
	if e != nil {
		return fail(LaunchRefused, "", e)
	}
	result.LogDirectory = dir
	stopToolEvents := monitorToolEvents(ctx, dir, l.OnToolEvent)
	defer stopToolEvents()
	defer func() {
		result.ProviderStats = readProviderStats(dir)
		raw, readErr := os.ReadFile(filepath.Join(dir, "filesystem-view.json"))
		if readErr == nil {
			var stats struct {
				ConstructionMS int64 `json:"construction_ms"`
				CopiedBytes    int64 `json:"copied_bytes"`
			}
			if json.Unmarshal(raw, &stats) == nil {
				result.ViewConstructionMS = stats.ConstructionMS
				result.ViewCopiedBytes = stats.CopiedBytes
			}
		}
	}()
	handoff := filepath.Join(dir, "handoff.json")
	config := filepath.Join(dir, "config.json")
	// Start from an allowlist, never inherit daemon credentials, Git overrides,
	// dynamic-loader injection, or shell startup configuration.
	stateKey := fmt.Sprintf("%x", sha256.Sum256([]byte(l.TaskID+"\x00"+l.Role+"\x00"+l.WorkflowID+"\x00"+l.WorkflowRevision+"\x00"+l.BranchInstance)))
	homeDir, tempDir := filepath.Join(d.Root, "provider-state", stateKey, "home"), filepath.Join(dir, "tmp")
	for _, path := range []string{homeDir, tempDir} {
		if e = os.MkdirAll(path, 0700); e != nil {
			return fail(LaunchRefused, handoff, e)
		}
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + homeDir, "TMPDIR=" + tempDir, "FAE_PROVIDER_STATE_HOME=" + homeDir}
	for k, v := range d.Environment {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) || strings.HasPrefix(k, "FAE_") || strings.HasPrefix(k, "DOLT_") || strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "DYLD_") || strings.HasPrefix(k, "LD_") || k == "HOME" || k == "TMPDIR" || k == "BASH_ENV" || k == "ENV" {
			return fail(LaunchRefused, handoff, fmt.Errorf("forbidden environment key %q", k))
		}
		env = append(env, k+"="+v)
	}
	l.CacheMounts, e = PrivateCacheDirectories(filepath.Dir(homeDir), l.CacheDirs)
	if e != nil {
		return fail(LaunchRefused, handoff, e)
	}
	env = ApplyPrivateCaches(env, l.CacheMounts)
	cacheInfo, _ := json.Marshal(l.CacheMounts)
	env = append(env, "FAE_CACHE_DIRS="+string(cacheInfo))
	forbiddenMounts := append([]string{l.Worktree, l.OutputDir, l.StagingDir, l.InstanceRoot, d.Root, homeDir}, l.ReadRootPaths...)
	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok && (key == "CLAUDE_CONFIG_DIR" || key == "CODEX_HOME") {
			forbiddenMounts = append(forbiddenMounts, value)
		}
	}
	if e = ValidateCacheDirectories(l.CacheDirs, append(append([]string{}, forbiddenMounts...), l.RuntimeMounts...)); e != nil {
		return fail(LaunchRefused, handoff, e)
	}
	result.RuntimeMounts, e = RuntimeMountIdentities(l.RuntimeMounts, forbiddenMounts)
	if e != nil {
		return fail(LaunchRefused, handoff, e)
	}
	for path, pinned := range l.RuntimeMountIdentities {
		if result.RuntimeMounts[path] != pinned {
			return fail(LaunchRefused, handoff, fmt.Errorf("runtime mount identity changed: %s", path))
		}
	}
	if l.IsolationRequired {
		result.IsolationPolicy = "namespace"
	} else {
		result.IsolationPolicy = "cli-and-collector"
		if l.ShellRuntimeMountsWithoutView {
			result.IsolationPolicy = "cli-runtime-mount-opt-in"
		}
	}
	// FAE-HOST-011 (2026-11-05 amendment) & FAE-RT-015: a tool-less session
	// (l.Session == "") has no shell tool and needs no role shims on PATH.
	// Every agentic session (l.Session == "agentic") that declares shell in its
	// tool profile places the role shim directory first on PATH and registers
	// the PreToolUse hook before launch; if either is missing, launch is refused.
	if l.Session != "" && l.Session != "agentic" {
		return fail(LaunchRefused, handoff, fmt.Errorf("unsupported session mode %q", l.Session))
	}
	var leaseProxy *commandshim.LeaseProxy
	if l.Session == "agentic" && AgenticShellAllowed(l.Profile) {
		policy := d.Shims
		policy.ShellAllow, policy.DenyTestRuns, policy.DeniedTestCommands, policy.ShellAuditPath = l.Profile.ShellAllow, l.Profile.DenyTestRuns, l.DeniedTestCommands, filepath.Join(dir, "shell-refusals.jsonl")
		policy.Worktree, policy.Local, policy.Build, policy.Test, policy.Format, policy.Shims = l.Worktree, l.Mode == "local", l.Profile.Build, l.Profile.Test, l.Profile.Format, l.Profile.Shims
		if l.IsolationRequired && (policy.Build || policy.Test || policy.Format) {
			policy.LeaseProxy = filepath.Join(dir, "host-lease.sock")
		}
		defer func() { _ = commandshim.RemoveGoRoot(dir) }()
		shims, e := commandshim.InstallRoleShims(dir, policy, commandshim.BasePath(env))
		if e != nil {
			return fail(LaunchRefused, handoff, e)
		}
		if policy.LeaseProxy != "" {
			installed, readErr := commandshim.ReadRoleShims(filepath.Join(shims, ".policy.json"))
			if readErr != nil {
				return fail(LaunchRefused, handoff, readErr)
			}
			leaseProxy, e = commandshim.StartLeaseProxy(ctx, policy.LeaseProxy, installed)
			if e != nil {
				return fail(LaunchRefused, handoff, e)
			}
			defer leaseProxy.Close()
		}
		if !d.OmitRoleShimsOnPath {
			env = commandshim.PrependPath(env, shims)
		}
		if !d.DisablePreToolHook && l.ClaudeSettings == "" {
			settingsPath, e := WriteClaudePreToolHook(dir, d.Shims.Broker)
			if e != nil {
				return fail(LaunchRefused, handoff, e)
			}
			l.ClaudeSettings = settingsPath
			if e := ConfigureClaudeTaskSettings(settingsPath, l); e != nil {
				return fail(LaunchRefused, handoff, e)
			}
		}
		if e := VerifyAgenticShellLaunch(env, l.ClaudeSettings); e != nil {
			return fail(LaunchRefused, handoff, e)
		}
		env = append(env, "CLAUDE_SETTINGS_PATH="+l.ClaudeSettings)
	}
	if l.Session == "agentic" && l.ClaudeSettings == "" {
		settingsPath, e := WriteClaudePreToolHook(dir, d.Shims.Broker)
		if e != nil {
			return fail(LaunchRefused, handoff, e)
		}
		l.ClaudeSettings = settingsPath
		if e := ConfigureClaudeTaskSettings(settingsPath, l); e != nil {
			return fail(LaunchRefused, handoff, e)
		}
	}
	if l.InstanceRoot == "" {
		l.InstanceRoot = d.Root
	}
	b, e := json.Marshal(BridgeConfig{Protocol: "fae-agent-v1", Launch: l, Handoff: handoff, LogDirectory: dir})
	if e != nil {
		return fail(LaunchRefused, handoff, e)
	}
	if e = os.WriteFile(config, b, 0600); e != nil {
		return fail(LaunchRefused, handoff, e)
	}
	timeout := d.Timeout
	if l.WallSeconds > 0 {
		timeout = time.Duration(l.WallSeconds) * time.Second
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	var wallBudgetExhausted atomic.Bool
	if l.TurnBudget != nil && l.TurnBudget.WallSeconds > 0 {
		timeout = time.Duration(l.TurnBudget.WallSeconds)*time.Second + 5*time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if l.TurnBudget != nil && l.TurnBudget.WallSeconds > 0 {
		wallTimer := time.AfterFunc(time.Duration(l.TurnBudget.WallSeconds)*time.Second, func() {
			wallBudgetExhausted.Store(true)
			cancel()
		})
		defer wallTimer.Stop()
	}
	args := append([]string(nil), d.Command...)
	for i, a := range args {
		if a == "{config}" {
			args[i] = config
		}
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = l.Worktree
	cmd.Env = env
	briefPath := filepath.Join(dir, "brief.json")
	if e = os.WriteFile(briefPath, l.Brief, 0600); e != nil {
		return fail(LaunchRefused, handoff, e)
	}
	briefFile, e := os.Open(briefPath)
	if e != nil {
		return fail(LaunchRefused, handoff, e)
	}
	defer briefFile.Close()
	cmd.Stdin = briefFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cap := d.TailBytes
	if cap <= 0 {
		cap = 64 << 10
	}
	tail := &tailBuffer{limit: cap}
	pipeRead, pipeWrite, e := os.Pipe()
	if e != nil {
		return fail(LaunchRefused, handoff, e)
	}
	defer pipeRead.Close()
	defer pipeWrite.Close()
	cmd.Stdout = pipeWrite
	cmd.Stderr = pipeWrite
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = time.Second
	// Bound transport startup independently from the full turn. The timer
	// cancels the same context so no abandoned startup goroutine can launch
	// later. It stops once the bridge runs; the provider's acknowledgement of
	// the prompt is bounded inside the bridge (its --start-timeout).
	startLimit := d.StartTimeout
	if startLimit <= 0 {
		startLimit = 180 * time.Second
	}
	startupTimer := time.AfterFunc(startLimit, cancel)
	defer startupTimer.Stop()
	// A missing/non-executable bridge is detected before the gated launcher.
	if _, e := exec.LookPath(args[0]); e != nil {
		result.NonExecution = true
		return fail("startup-not-executed", handoff, e)
	}
	var hooks *processHooks
	if d.prepareProcess != nil {
		hooks, e = d.prepareProcess(ctx, cmd, dir)
		if e != nil {
			result.NonExecution = true
			return fail("startup-not-executed", handoff, e)
		}
		hooksRetired = false
		defer func() { e := hooks.Close(); hooksRetired = e == nil; err = errors.Join(err, e) }()
	}
	record, e := processowner.StartRole(cmd, filepath.Join(d.Root, ".processes"), d.Root, l.SessionID, sessionRole(hooks))
	if e != nil {
		// An unreleased gate never exec'd the bridge; its proven retirement
		// lets a cancelled launch retire its session lease (FAE-ROB-011).
		var gate *processowner.GateError
		unreleased := errors.As(e, &gate)
		result.NonExecution = cmd.Process == nil || unreleased
		groupRetired = cmd.Process == nil || unreleased && gate.Retired
		return fail("startup-not-executed", handoff, e)
	}
	defer func() { e := processowner.Retire(record); groupRetired = e == nil; err = errors.Join(err, e) }()
	pid := cmd.Process.Pid
	if leaseProxy != nil {
		leaseProxy.BindProcess(pid)
	}
	session := &liveSession{pid: pid, task: l.TaskID, role: l.Role, started: startedAt, cancel: cancel, done: make(chan struct{})}
	d.mu.Lock()
	d.live[l.SessionID] = session
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.live, l.SessionID)
		d.mu.Unlock()
		close(session.done)
	}()
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(tail, pipeRead); close(drained) }()
	if hooks != nil {
		if e = hooks.Started(); e != nil {
			if hooks.NonExecution != nil {
				result.NonExecution = hooks.NonExecution()
			}
			_ = cmd.Cancel()
			_ = processowner.Wait(cmd, record)
			return fail("startup-not-executed", handoff, e)
		}
	}
	startupTimer.Stop()
	d.mu.Lock()
	session.ready = true
	d.mu.Unlock()
	stallLimit := d.StallTimeout
	if stallLimit <= 0 {
		stallLimit = 5 * time.Minute
	}
	interval := d.StallPollInterval
	if interval <= 0 {
		interval = time.Second
	}
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	stallDone := make(chan bool, 1)
	activityPID := func() int {
		if hooks != nil && hooks.ActivityPID != nil {
			return hooks.ActivityPID()
		}
		return pid
	}
	go monitorStall(monitorCtx, activityPID, dir, handoff, stallLimit, interval, cancel, stallDone)
	var cpDone chan struct{}
	if l.Session == "agentic" && l.OnCheckpoint != nil {
		cpDone = make(chan struct{})
		go func() {
			defer close(cpDone)
			var last TurnCheckpoint
			check := func() {
				cp, readErr := ReadTurnCheckpoint(dir)
				if readErr != nil || cp == nil || (cp.ProviderSession == "" && cp.WorktreeTree == "") {
					return
				}
				if *cp != last {
					last = *cp
					l.OnCheckpoint(*cp)
				}
			}
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-monitorCtx.Done():
					check()
					return
				case <-ticker.C:
					check()
				}
			}
		}()
	}
	e = processowner.Wait(cmd, record)
	stopMonitor()
	if cpDone != nil {
		<-cpDone
	}
	stalled := <-stallDone
	if hooks != nil {
		e = hooks.Finished(e)
	}
	_ = pipeWrite.Close()
	select {
	case <-drained:
	case <-time.After(time.Second):
		_ = pipeRead.Close()
		<-drained
	}
	result.Tail = tail.String()
	if stalled {
		return fail("stalled", handoff, errors.New("positive liveness signals unchanged beyond stall threshold"))
	}
	if wallBudgetExhausted.Load() {
		refusal := &BridgeRefusal{Class: TurnBudgetExhausted, Detail: fmt.Sprintf("agentic turn wall_seconds budget (%ds) exhausted", l.TurnBudget.WallSeconds)}
		return fail(TurnBudgetExhausted, handoff, refusal)
	}
	if ctx.Err() != nil {
		return fail("interrupted", handoff, ctx.Err())
	}
	if e != nil {
		if refusal := collectBridgeRefusal(e, dir, handoff, l, startedAt); refusal != nil {
			return fail(refusal.Class, handoff, refusal)
		}
		return fail("process-failed", handoff, e)
	}
	f, e := os.OpenFile(handoff, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		if cause := providerhealth.SessionLogs(result.LogDirectory); cause != providerhealth.OK {
			e = fmt.Errorf("provider cause %s: %w", cause, e)
		}
		return fail("missing-handoff", handoff, e)
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return fail("malformed-output", handoff, e)
	}
	max := d.MaxHandoffBytes
	if max <= 0 {
		max = 1 << 20
	}
	if st.ModTime().Before(startedAt) {
		return fail("stale-handoff", handoff, errors.New("handoff predates launch"))
	}
	if !st.Mode().IsRegular() || st.Size() > max {
		return fail("malformed-output", handoff, errors.New("handoff must be bounded regular file"))
	}
	raw, e := io.ReadAll(io.LimitReader(f, max+1))
	if e != nil || int64(len(raw)) > max {
		return fail("malformed-output", handoff, errors.New("handoff exceeds bound"))
	}
	raw, e = domain.CanonicalJSON(raw)
	if e != nil {
		return fail("malformed-output", handoff, e)
	}
	var h Handoff
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if e = dec.Decode(&h); e != nil {
		return fail("malformed-output", handoff, e)
	}
	if h.DispatchID == "" {
		return fail("id-less-handoff", handoff, errors.New("dispatch_id required"))
	}
	if h.DispatchID != l.DispatchID || h.SessionID != l.SessionID || h.TaskID != l.TaskID {
		return fail("stale-handoff", handoff, errors.New("identity mismatch"))
	}
	if len(h.Output) == 0 {
		return fail("malformed-output", handoff, errors.New("output required"))
	}
	result.Output = h.Output
	result.AssistantTurns = h.AssistantTurns
	result.ProviderSession = h.ProviderSession
	result.CheckpointTree = h.CheckpointTree
	result.ResumedFromCheckpoint = h.ResumedFromCheckpoint
	result.ProducedAt = time.Now().UTC()
	return result, nil
}

type tailBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n >= t.limit {
		t.data = append(t.data[:0], p[n-t.limit:]...)
	} else {
		t.data = append(t.data, p...)
		if len(t.data) > t.limit {
			t.data = append([]byte(nil), t.data[len(t.data)-t.limit:]...)
		}
	}
	return n, nil
}
func (t *tailBuffer) String() string { t.mu.Lock(); defer t.mu.Unlock(); return string(t.data) }

// processHooks changes only transport; Direct retains handoff and identity validation.
type processHooks struct {
	NonExecution func() bool
	ActivityPID  func() int
	Started      func() error
	Finished     func(error) error
	Close        func() error
}

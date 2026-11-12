package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/symphony"
	"github.com/xleelz/fae/tracker"
)

const (
	defaultPollIntervalMS      = 30000
	defaultMaxConcurrentAgents = 10
	defaultMaxRetryBackoffMS   = 300000
	defaultStallTimeoutMS      = 300000
	continuationRetryDelayMS   = 1000
	baseFailureRetryDelayMS    = 10000
	maxRecentEventsPerIssue    = 50
)

// SanitizeWorkspaceKey delegates to symphony.WorkspaceKey so sanitized keys always include the
// collision-resistant 16-hex-char SHA-256 suffix when sanitization alters identifier (Symphony §4.2).
func SanitizeWorkspaceKey(identifier string) string {
	return symphony.WorkspaceKey(identifier)
}

// EnsureIssueWorkspace creates or reuses <root>/<sanitized_identifier> while enforcing path containment (Symphony §9).
func EnsureIssueWorkspace(root string, identifier string) (symphony.Workspace, error) {
	if strings.TrimSpace(root) == "" {
		return symphony.Workspace{}, errors.New("workspace root is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return symphony.Workspace{}, fmt.Errorf("resolve workspace root: %w", err)
	}
	key := SanitizeWorkspaceKey(identifier)
	wsPath := filepath.Join(absRoot, key)
	rel, err := filepath.Rel(absRoot, wsPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return symphony.Workspace{}, fmt.Errorf("workspace path %q escapes root %q", wsPath, absRoot)
	}
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return symphony.Workspace{}, fmt.Errorf("create workspace root: %w", err)
	}
	createdNow := false
	info, statErr := os.Lstat(wsPath)
	if os.IsNotExist(statErr) {
		if err := os.MkdirAll(wsPath, 0o755); err != nil {
			return symphony.Workspace{}, fmt.Errorf("create issue workspace: %w", err)
		}
		createdNow = true
	} else if statErr != nil {
		return symphony.Workspace{}, fmt.Errorf("stat issue workspace: %w", statErr)
	} else if !info.IsDir() {
		return symphony.Workspace{}, fmt.Errorf("workspace path %q exists and is not a directory", wsPath)
	}
	return symphony.Workspace{
		Path:         wsPath,
		WorkspaceKey: key,
		CreatedNow:   createdNow,
	}, nil
}

// RemoveIssueWorkspace removes the workspace directory for identifier under root if it exists (Symphony §8.6, §9).
func RemoveIssueWorkspace(root string, identifier string) error {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(identifier) == "" {
		return nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	key := SanitizeWorkspaceKey(identifier)
	wsPath := filepath.Join(absRoot, key)
	rel, err := filepath.Rel(absRoot, wsPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to remove path %q outside workspace root %q", wsPath, absRoot)
	}
	return os.RemoveAll(wsPath)
}

// LinearPriorityToFae maps Linear priority (1..4 where 1 is Urgent) to Fae priority (>= 10, higher first).
func LinearPriorityToFae(priority *int) int {
	if priority == nil {
		return 10
	}
	switch *priority {
	case 1:
		return 40
	case 2:
		return 30
	case 3:
		return 20
	case 4:
		return 10
	default:
		return 10
	}
}

// BackoffDelayMS computes exponential retry backoff capped at maxBackoffMS (Symphony §8.4).
func BackoffDelayMS(attempt int, maxBackoffMS int) int {
	if maxBackoffMS <= 0 {
		maxBackoffMS = defaultMaxRetryBackoffMS
	}
	if attempt < 1 {
		attempt = 1
	}
	exp := attempt - 1
	if exp > 20 {
		exp = 20
	}
	delay := float64(baseFailureRetryDelayMS) * math.Pow(2, float64(exp))
	if delay > float64(maxBackoffMS) {
		return maxBackoffMS
	}
	return int(delay)
}

// TokenCounts holds input, output, and total token counts (Symphony §13.5).
type TokenCounts struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// TokenUsagePayload represents structured token usage payloads emitted by coding agents (Symphony §13.5).
type TokenUsagePayload struct {
	TotalTokenUsage *TokenCounts `json:"total_token_usage,omitempty"`
	LastTokenUsage  *TokenCounts `json:"last_token_usage,omitempty"`
	InputTokens     int64        `json:"input_tokens,omitempty"`
	OutputTokens    int64        `json:"output_tokens,omitempty"`
	TotalTokens     int64        `json:"total_tokens,omitempty"`
}

// AgentEvent represents a structured coding-agent runtime event (Symphony §10.4, §13.5).
type AgentEvent struct {
	Event             string             `json:"event"`
	Timestamp         time.Time          `json:"timestamp"`
	Message           string             `json:"message,omitempty"`
	CodexAppServerPID string             `json:"codex_app_server_pid,omitempty"`
	SessionID         string             `json:"session_id,omitempty"`
	ThreadID          string             `json:"thread_id,omitempty"`
	TurnID            string             `json:"turn_id,omitempty"`
	Usage             *TokenUsagePayload `json:"usage,omitempty"`
	RateLimits        any                `json:"rate_limits,omitempty"`
	Raw               map[string]any     `json:"raw,omitempty"`
}

// CodexTotals tracks aggregate token and runtime metrics across the orchestrator lifetime (Symphony §4.1.8).
type CodexTotals struct {
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	TotalTokens    int64   `json:"total_tokens"`
	SecondsRunning float64 `json:"seconds_running"`
}

// RecentEventEntry stores a bounded per-issue event history item for observability.
type RecentEventEntry struct {
	At      time.Time `json:"at"`
	Event   string    `json:"event"`
	Message string    `json:"message,omitempty"`
}

// LinearIssueRecord persists normalized Linear issue state and parent/sub-issue hierarchy in memory/sync state.
type LinearIssueRecord struct {
	Issue      symphony.Issue     `json:"issue"`
	IssueID    string             `json:"issue_id"`
	Identifier string             `json:"identifier"`
	Title      string             `json:"title"`
	State      string             `json:"state"`
	TaskID     string             `json:"task_id,omitempty"`
	Stage      string             `json:"stage,omitempty"`
	Parent     *tracker.IssueRef  `json:"parent,omitempty"`
	Children   []tracker.IssueRef `json:"children,omitempty"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

// RunningIssueEntry is the authoritative live state for an actively executing Linear issue (Symphony §4.1.6).
type RunningIssueEntry struct {
	Issue                    symphony.Issue     `json:"issue"`
	IssueID                  string             `json:"issue_id"`
	Identifier               string             `json:"identifier"`
	TaskID                   string             `json:"task_id,omitempty"`
	Stage                    string             `json:"stage,omitempty"`
	Parent                   *tracker.IssueRef  `json:"parent,omitempty"`
	Children                 []tracker.IssueRef `json:"children,omitempty"`
	WorkspacePath            string             `json:"workspace_path"`
	SSHHost                  string             `json:"ssh_host,omitempty"`
	SessionID                string             `json:"session_id"`
	ThreadID                 string             `json:"thread_id,omitempty"`
	TurnID                   string             `json:"turn_id,omitempty"`
	CodexAppServerPID        string             `json:"codex_app_server_pid,omitempty"`
	TurnCount                int                `json:"turn_count"`
	RetryAttempt             *int               `json:"retry_attempt"`
	StartedAt                time.Time          `json:"started_at"`
	LastEvent                string             `json:"last_event,omitempty"`
	LastMessage              string             `json:"last_message"`
	LastEventAt              *time.Time         `json:"last_event_at"`
	InputTokens              int64              `json:"input_tokens"`
	OutputTokens             int64              `json:"output_tokens"`
	TotalTokens              int64              `json:"total_tokens"`
	LastReportedInputTokens  int64              `json:"last_reported_input_tokens"`
	LastReportedOutputTokens int64              `json:"last_reported_output_tokens"`
	LastReportedTotalTokens  int64              `json:"last_reported_total_tokens"`
	Cancel                   context.CancelFunc `json:"-"`
}

// RetryEntry is the scheduled retry state for an issue (Symphony §4.1.7).
type RetryEntry struct {
	IssueID    string      `json:"issue_id"`
	Identifier string      `json:"identifier"`
	Attempt    int         `json:"attempt"`
	DueAt      time.Time   `json:"due_at"`
	DueAtMS    int64       `json:"due_at_ms"`
	Error      string      `json:"error,omitempty"`
	Timer      *time.Timer `json:"-"`
}

// StageTransitionRecord records a Fae workflow stage transition synchronized to Linear.
type StageTransitionRecord struct {
	TaskID      string    `json:"task_id"`
	IssueID     string    `json:"issue_id"`
	Stage       string    `json:"stage"`
	TaskState   string    `json:"task_state"`
	LinearState string    `json:"linear_state"`
	Summary     string    `json:"summary"`
	CommentID   string    `json:"comment_id,omitempty"`
	At          time.Time `json:"at"`
}

// TaskBridge abstracts hybrid Fae backlog admission and task lifecycle control.
type TaskBridge interface {
	AdmitLinearIssue(ctx context.Context, issue symphony.Issue, workspacePath string, title string, renderedPrompt string, priority int) (taskID string, err error)
	StopLinearTask(ctx context.Context, taskID string, reason string) error
}

// DaemonTaskBridge implements TaskBridge directly against a Fae *Daemon and its Dolt store.
type DaemonTaskBridge struct {
	daemon *Daemon
	tree   string
	config symphony.FaeConfig
}

// NewDaemonTaskBridge wraps d as a hybrid Fae TaskBridge.
func NewDaemonTaskBridge(d *Daemon) *DaemonTaskBridge {
	bridge := &DaemonTaskBridge{daemon: d}
	if d != nil && d.config.Symphony != nil {
		bridge.config = d.config.Symphony.ServiceConfig.Fae
	}
	return bridge
}

// LinearTaskBridge returns a TaskBridge backed by this Daemon.
func (d *Daemon) LinearTaskBridge() *DaemonTaskBridge {
	return NewDaemonTaskBridge(d)
}

// AdmitLinearIssue admits or updates a Fae task in Dolt corresponding to a Linear issue.
func (b *DaemonTaskBridge) AdmitLinearIssue(ctx context.Context, issue symphony.Issue, workspacePath string, title string, renderedPrompt string, priority int) (string, error) {
	if b == nil || b.daemon == nil {
		return "", errors.New("daemon task bridge is nil")
	}
	return b.admitLinearIssueDurable(ctx, issue, workspacePath, title, renderedPrompt, priority)
}

// StopLinearTask cancels or retires an in-flight Fae task when Linear reconciliation stops it.
func (b *DaemonTaskBridge) StopLinearTask(ctx context.Context, taskID string, reason string) error {
	if b == nil || b.daemon == nil || strings.TrimSpace(taskID) == "" {
		return nil
	}
	d := b.daemon
	d.mu.Lock()
	defer d.mu.Unlock()
	snap, ok, err := d.storedJob(ctx, taskID)
	if err != nil || !ok || backlogTerminal(snap.State) {
		return err
	}
	audit := controlplane.Audit{
		Actor:   "symphony",
		Action:  "LinearStop",
		Outcome: "ok",
	}
	if snap.State == "Pending" || snap.State == "Blocked" {
		raw, _ := json.Marshal(TaskControlParams{
			ID:     taskID,
			Reason: reason,
		})
		_, err = d.taskControl(ctx, "Cancel", raw, audit)
		return err
	}
	if snap.State != "Escalated" {
		escRaw, _ := json.Marshal(TaskControlParams{
			ID:     taskID,
			Reason: reason,
		})
		_, _ = d.taskControl(ctx, "Escalate", escRaw, audit)
	}
	retireRaw, _ := json.Marshal(TaskControlParams{
		ID:     taskID,
		Reason: "obsolete: stopped by Linear tracker reconciliation",
	})
	_, err = d.taskControl(ctx, "Retire", retireRaw, audit)
	return err
}

// OrchestratorConfig configures a LinearOrchestrator instance.
type OrchestratorConfig struct {
	AdapterFactory            func(symphony.ServiceConfig) (tracker.Adapter, error)
	Adapter                   tracker.Adapter
	ServiceConfig             symphony.ServiceConfig
	PromptTemplate            string
	ConfigManager             *symphony.ConfigManager
	ValidateConfig            func() error
	Bridge                    TaskBridge
	WorkerLauncher            func(ctx context.Context, issue symphony.Issue, ws symphony.Workspace, prompt string, attempt *int) error
	EnableAppServerRunner     bool
	TransitionStateOnDispatch string
	PostStartComment          bool
	EnableRetryTimers         bool
	Logger                    func(format string, args ...any)
	Now                       func() time.Time
}

// LinearOrchestrator owns the authoritative in-memory scheduling, reconciliation, retry,
// and hybrid Fae task synchronization state (Symphony §§4.1.8, 7, 8, 13.5, 16).
type LinearOrchestrator struct {
	adapterFactory             func(symphony.ServiceConfig) (tracker.Adapter, error)
	mu                         sync.RWMutex
	adapter                    tracker.Adapter
	serviceConfig              symphony.ServiceConfig
	promptTemplate             string
	ConfigManager              *symphony.ConfigManager
	validateConfig             func() error
	bridge                     TaskBridge
	workerLauncher             func(ctx context.Context, issue symphony.Issue, ws symphony.Workspace, prompt string, attempt *int) error
	enableAppServerRunner      bool
	transitionStateOnDispatch  string
	postStartComment           bool
	enableRetryTimers          bool
	Logger                     func(format string, args ...any)
	now                        func() time.Time
	hasCustomStallTimeout      bool
	PollIntervalMS             int
	MaxConcurrentAgents        int
	MaxConcurrentAgentsByState map[string]int
	MaxRetryBackoffMS          int
	StallTimeoutMS             int
	WorkspaceRoot              string
	RequiredLabels             []string

	Running       map[string]*RunningIssueEntry
	Claimed       map[string]bool
	RetryAttempts map[string]*RetryEntry
	Completed     map[string]bool
	CodexTotals   CodexTotals
	RateLimits    any

	RecentEvents  map[string][]RecentEventEntry
	LastError     map[string]string
	RestartCounts map[string]int
	LastSSHHost   map[string]string
	KnownIssues   map[string]symphony.Issue
	IssueRecords  map[string]LinearIssueRecord
	TaskToIssue   map[string]string
	IssueToTask   map[string]string
	StageHistory  map[string][]StageTransitionRecord
	LastTickError string

	bridgeTasks       map[string]LinearTaskStatus
	tickMu            sync.Mutex
	loopContext       context.Context
	refreshMu         sync.Mutex
	refreshInProgress bool
	refreshPending    bool
}

// NewLinearOrchestrator creates and initializes a LinearOrchestrator.
func NewLinearOrchestrator(cfg OrchestratorConfig) *LinearOrchestrator {
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	pollMS := cfg.ServiceConfig.Polling.IntervalMS
	if pollMS <= 0 {
		pollMS = defaultPollIntervalMS
	}
	maxAgents := cfg.ServiceConfig.Agent.MaxConcurrentAgents
	if maxAgents <= 0 {
		maxAgents = defaultMaxConcurrentAgents
	}
	maxBackoff := cfg.ServiceConfig.Agent.MaxRetryBackoffMS
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxRetryBackoffMS
	}
	stallMS := cfg.ServiceConfig.Codex.StallTimeoutMS
	wsRoot := strings.TrimSpace(cfg.ServiceConfig.Workspace.Root)
	if wsRoot == "" {
		wsRoot = filepath.Join(os.TempDir(), "symphony_workspaces")
	}
	byState := make(map[string]int)
	for k, v := range cfg.ServiceConfig.Agent.MaxConcurrentAgentsByState {
		norm := symphony.NormalizeState(k)
		if norm != "" && v > 0 {
			byState[norm] = v
		}
	}
	reqLabels := append([]string(nil), cfg.ServiceConfig.Tracker.RequiredLabels...)
	transitionOnDispatch := strings.TrimSpace(cfg.TransitionStateOnDispatch)
	if transitionOnDispatch == "" && cfg.Bridge != nil {
		transitionOnDispatch = "In Progress"
	}
	postStart := cfg.PostStartComment || cfg.Bridge != nil
	if _, embedded := cfg.Bridge.(linearTaskStateBridge); embedded {
		transitionOnDispatch = ""
		if cfg.ServiceConfig.Fae.SyncState {
			transitionOnDispatch = "In Progress"
		}
		postStart = cfg.ServiceConfig.Fae.PostComments
	}

	return &LinearOrchestrator{
		adapter:                    cfg.Adapter,
		serviceConfig:              cfg.ServiceConfig,
		adapterFactory:             cfg.AdapterFactory,
		promptTemplate:             cfg.PromptTemplate,
		ConfigManager:              cfg.ConfigManager,
		validateConfig:             cfg.ValidateConfig,
		bridge:                     cfg.Bridge,
		workerLauncher:             cfg.WorkerLauncher,
		enableAppServerRunner:      cfg.EnableAppServerRunner,
		transitionStateOnDispatch:  transitionOnDispatch,
		postStartComment:           postStart,
		enableRetryTimers:          cfg.EnableRetryTimers,
		Logger:                     cfg.Logger,
		now:                        nowFn,
		PollIntervalMS:             pollMS,
		MaxConcurrentAgents:        maxAgents,
		MaxConcurrentAgentsByState: byState,
		MaxRetryBackoffMS:          maxBackoff,
		StallTimeoutMS:             stallMS,
		WorkspaceRoot:              wsRoot,
		RequiredLabels:             reqLabels,
		Running:                    make(map[string]*RunningIssueEntry),
		Claimed:                    make(map[string]bool),
		RetryAttempts:              make(map[string]*RetryEntry),
		Completed:                  make(map[string]bool),
		RecentEvents:               make(map[string][]RecentEventEntry),
		LastError:                  make(map[string]string),
		RestartCounts:              make(map[string]int),
		LastSSHHost:                make(map[string]string),
		KnownIssues:                make(map[string]symphony.Issue),
		IssueRecords:               make(map[string]LinearIssueRecord),
		TaskToIssue:                make(map[string]string),
		IssueToTask:                make(map[string]string),
		StageHistory:               make(map[string][]StageTransitionRecord),
	}
}

// UpdateConfig dynamically updates effective orchestrator configuration and prompt template in-place (Symphony §6.2).
func (o *LinearOrchestrator) UpdateConfig(svcCfg symphony.ServiceConfig, promptTemplate string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.applyConfigLocked(svcCfg, promptTemplate)
}

func (o *LinearOrchestrator) applyConfigLocked(svcCfg symphony.ServiceConfig, promptTemplate string) {
	if _, embedded := o.bridge.(linearTaskStateBridge); embedded {
		o.transitionStateOnDispatch = ""
		if svcCfg.Fae.SyncState {
			o.transitionStateOnDispatch = "In Progress"
		}
		o.postStartComment = svcCfg.Fae.PostComments
	}
	o.serviceConfig = svcCfg
	o.promptTemplate = promptTemplate
	if svcCfg.Polling.IntervalMS > 0 {
		o.PollIntervalMS = svcCfg.Polling.IntervalMS
	}
	if svcCfg.Agent.MaxConcurrentAgents > 0 {
		o.MaxConcurrentAgents = svcCfg.Agent.MaxConcurrentAgents
	}
	if svcCfg.Agent.MaxRetryBackoffMS > 0 {
		o.MaxRetryBackoffMS = svcCfg.Agent.MaxRetryBackoffMS
	}
	o.StallTimeoutMS = svcCfg.Codex.StallTimeoutMS
	if wsRoot := strings.TrimSpace(svcCfg.Workspace.Root); wsRoot != "" {
		o.WorkspaceRoot = wsRoot
	}
	byState := make(map[string]int)
	for k, v := range svcCfg.Agent.MaxConcurrentAgentsByState {
		norm := symphony.NormalizeState(k)
		if norm != "" && v > 0 {
			byState[norm] = v
		}
	}
	o.MaxConcurrentAgentsByState = byState
	o.RequiredLabels = append([]string(nil), svcCfg.Tracker.RequiredLabels...)
}

func (o *LinearOrchestrator) logf(format string, args ...any) {
	if o.Logger == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	o.Logger(format, args...)
}

func (o *LinearOrchestrator) logStructured(event string, issueID string, identifier string, sessionID string, kvPairs ...string) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "event=%s issue_id=%s issue_identifier=%s session_id=%s",
		event, issueID, identifier, sessionID)
	for _, kv := range kvPairs {
		if strings.TrimSpace(kv) != "" {
			sb.WriteString(" ")
			sb.WriteString(kv)
		}
	}
	o.logf("%s", sb.String())
}

func (o *LinearOrchestrator) workspaceManagerLocked() (*symphony.WorkspaceManager, error) {
	wm, err := symphony.NewWorkspaceManager(o.WorkspaceRoot, o.serviceConfig.Hooks)
	if err != nil {
		return nil, err
	}
	wm.Logger = o.logf
	return wm, nil
}

func (o *LinearOrchestrator) upsertIssueRecordLocked(issue symphony.Issue, taskID string, stage string) LinearIssueRecord {
	if o.IssueRecords == nil {
		o.IssueRecords = make(map[string]LinearIssueRecord)
	}
	parent, children := tracker.ExtractHierarchy(issue)
	prev := o.IssueRecords[issue.ID]
	if parent == nil && prev.Parent != nil {
		cp := *prev.Parent
		parent = &cp
	}
	if len(children) == 0 && len(prev.Children) > 0 {
		children = append([]tracker.IssueRef(nil), prev.Children...)
	}
	if parent != nil || len(children) > 0 {
		issue = tracker.WithHierarchy(issue, parent, children)
	}
	if taskID == "" {
		if mapped := o.IssueToTask[issue.ID]; mapped != "" {
			taskID = mapped
		} else {
			taskID = prev.TaskID
		}
	}
	if stage == "" {
		stage = prev.Stage
	}
	rec := LinearIssueRecord{
		Issue:      issue,
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		Title:      issue.Title,
		State:      issue.State,
		TaskID:     taskID,
		Stage:      stage,
		Parent:     parent,
		Children:   children,
		UpdatedAt:  o.now().UTC(),
	}
	o.IssueRecords[issue.ID] = rec
	o.KnownIssues[issue.ID] = issue
	if rec.Parent != nil {
		o.syncChildStateIntoParentLocked(rec)
	}
	return rec
}

func (o *LinearOrchestrator) syncChildStateIntoParentLocked(childRec LinearIssueRecord) {
	if childRec.Parent == nil {
		return
	}
	parentKey := strings.TrimSpace(childRec.Parent.ID)
	if _, ok := o.IssueRecords[parentKey]; !ok && childRec.Parent.Identifier != "" {
		for k, pr := range o.IssueRecords {
			if strings.EqualFold(pr.Identifier, childRec.Parent.Identifier) {
				parentKey = k
				break
			}
		}
	}
	parentRec, ok := o.IssueRecords[parentKey]
	if !ok {
		return
	}
	updated := false
	for i := range parentRec.Children {
		ch := &parentRec.Children[i]
		if (childRec.IssueID != "" && ch.ID == childRec.IssueID) ||
			(childRec.Identifier != "" && strings.EqualFold(ch.Identifier, childRec.Identifier)) {
			ch.State = childRec.State
			if childRec.Title != "" {
				ch.Title = childRec.Title
			}
			updated = true
		}
	}
	if updated {
		parentRec.Issue = tracker.WithHierarchy(parentRec.Issue, parentRec.Parent, parentRec.Children)
		parentRec.UpdatedAt = o.now().UTC()
		o.IssueRecords[parentKey] = parentRec
		o.KnownIssues[parentKey] = parentRec.Issue
		if entry, running := o.Running[parentKey]; running && entry != nil {
			entry.Children = append([]tracker.IssueRef(nil), parentRec.Children...)
			entry.Issue = parentRec.Issue
		}
	}
}

// IssueRecord returns the synced LinearIssueRecord (including Parent and Children hierarchy) by issue ID or identifier.
func (o *LinearOrchestrator) IssueRecord(idOrIdentifier string) (LinearIssueRecord, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	key := strings.TrimSpace(idOrIdentifier)
	if key == "" {
		return LinearIssueRecord{}, false
	}
	if rec, ok := o.IssueRecords[key]; ok {
		return rec, true
	}
	for _, rec := range o.IssueRecords {
		if strings.EqualFold(rec.Identifier, key) {
			return rec, true
		}
	}
	if iss, ok := o.KnownIssues[key]; ok {
		parent, children := tracker.ExtractHierarchy(iss)
		return LinearIssueRecord{
			Issue:      iss,
			IssueID:    iss.ID,
			Identifier: iss.Identifier,
			Title:      iss.Title,
			State:      iss.State,
			TaskID:     o.IssueToTask[iss.ID],
			Parent:     parent,
			Children:   children,
		}, true
	}
	for _, iss := range o.KnownIssues {
		if strings.EqualFold(iss.Identifier, key) {
			parent, children := tracker.ExtractHierarchy(iss)
			return LinearIssueRecord{
				Issue:      iss,
				IssueID:    iss.ID,
				Identifier: iss.Identifier,
				Title:      iss.Title,
				State:      iss.State,
				TaskID:     o.IssueToTask[iss.ID],
				Parent:     parent,
				Children:   children,
			}, true
		}
	}
	return LinearIssueRecord{}, false
}

// ValidateDispatchConfig performs preflight validation before dispatch (Symphony §6.3).
func (o *LinearOrchestrator) ValidateDispatchConfig() error {
	o.mu.RLock()
	validateFn := o.validateConfig
	adapter := o.adapter
	o.mu.RUnlock()

	if validateFn != nil {
		if err := validateFn(); err != nil {
			return err
		}
	}
	if adapter == nil {
		return errors.New("missing tracker adapter")
	}
	if strings.TrimSpace(adapter.Kind()) == "" {
		return errors.New("tracker.kind is required")
	}
	if len(adapter.ActiveStates()) == 0 {
		return errors.New("tracker.active_states must not be empty")
	}
	return nil
}

// StartupCleanup removes workspace directories for issues already in terminal states at startup (Symphony §8.6).
// If fetching terminal issues fails, it logs a warning and continues startup without returning an error.
func (o *LinearOrchestrator) StartupCleanup(ctx context.Context) {
	if _, embedded := o.bridge.(linearTaskStateBridge); embedded {
		return
	}
	o.mu.RLock()
	adapter := o.adapter
	wsRoot := o.WorkspaceRoot
	wm, _ := o.workspaceManagerLocked()
	o.mu.RUnlock()

	if adapter == nil {
		return
	}
	termStates := adapter.TerminalStates()
	if len(termStates) == 0 {
		return
	}
	issues, err := adapter.FetchIssuesByStates(ctx, termStates)
	if err != nil {
		o.mu.Lock()
		o.LastTickError = fmt.Sprintf("startup terminal cleanup warning: %v", err)
		o.mu.Unlock()
		o.logf("event=startup_cleanup_failed error=%q", err.Error())
		return
	}
	for _, issue := range issues {
		if wm != nil {
			_ = wm.CleanupIssueWorkspace(ctx, issue.Identifier)
		} else {
			_ = RemoveIssueWorkspace(wsRoot, issue.Identifier)
		}
	}
}

func (o *LinearOrchestrator) reloadConfigIfChanged() error {
	o.mu.RLock()
	cm := o.ConfigManager
	o.mu.RUnlock()
	if cm == nil {
		return nil
	}
	changed, err := cm.ReloadIfChanged()
	if err != nil {
		o.mu.Lock()
		o.LastTickError = fmt.Sprintf("workflow reload failed: %v", err)
		o.mu.Unlock()
		o.logf("event=workflow_reload_failed error=%q", err.Error())
		return err
	}
	if changed {
		if cfg := cm.Config(); cfg != nil {
			tmpl := ""
			if wf := cm.Workflow(); wf != nil {
				tmpl = wf.PromptTemplate
			}
			var replacement tracker.Adapter
			o.mu.RLock()
			factory := o.adapterFactory
			o.mu.RUnlock()
			if factory != nil {
				if err := validateEmbeddedLinear(*cfg); err != nil {
					return o.bridgeError(err)
				}
				var err error
				replacement, err = factory(*cfg)
				if err != nil {
					return o.bridgeError(err)
				}
			}
			o.mu.Lock()
			o.applyConfigLocked(*cfg, tmpl)
			if replacement != nil {
				o.adapter = replacement
			}
			if bridge, ok := o.bridge.(*DaemonTaskBridge); ok {
				bridge.config = cfg.Fae
			}
			o.mu.Unlock()
			o.logf("event=workflow_reloaded poll_interval_ms=%d max_concurrent_agents=%d", cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents)
		}
	}
	return nil
}

// Tick runs one full reconciliation + preflight validation + poll + sort + dispatch cycle (Symphony §8.1, §16.2).
func (o *LinearOrchestrator) Tick(ctx context.Context) error {
	o.tickMu.Lock()
	defer o.tickMu.Unlock()
	reloadErr := o.reloadConfigIfChanged()
	if err := o.reconcileBridgeTasks(ctx); err != nil {
		return err
	}

	o.ReconcileRunningIssues(ctx)

	if err := o.ValidateDispatchConfig(); err != nil {
		o.mu.Lock()
		o.LastTickError = err.Error()
		o.mu.Unlock()
		return err
	}

	o.mu.RLock()
	adapter := o.adapter
	o.mu.RUnlock()

	issues, err := adapter.FetchIssuesByStates(ctx, adapter.ActiveStates())
	if err != nil {
		o.mu.Lock()
		o.LastTickError = err.Error()
		o.mu.Unlock()
		return err
	}

	if err := o.restoreBridgeCandidates(ctx, issues); err != nil {
		return err
	}
	sorted := symphony.SortIssuesForDispatch(issues)

	o.mu.Lock()
	if reloadErr == nil {
		o.LastTickError = ""
	}
	for _, issue := range sorted {
		if issue.ID != "" {
			o.upsertIssueRecordLocked(issue, "", "")
		}
		if !o.hasAvailableGlobalSlotLocked() {
			break
		}
		if o.shouldDispatchLocked(issue) {
			_ = o.dispatchIssueLocked(ctx, issue, nil)
		}
	}
	o.mu.Unlock()
	return nil
}

// ReconcileRunningIssues performs stall detection and tracker state refresh for all active issues (Symphony §8.5, §16.3).
func (o *LinearOrchestrator) ReconcileRunningIssues(ctx context.Context) {
	o.mu.Lock()
	now := o.now().UTC()
	if _, embedded := o.bridge.(linearTaskStateBridge); o.StallTimeoutMS > 0 && !embedded {
		var stalledIDs []string
		for id, entry := range o.Running {
			ref := entry.StartedAt
			if entry.LastEventAt != nil && !entry.LastEventAt.IsZero() {
				ref = *entry.LastEventAt
			}
			elapsedMS := now.Sub(ref).Milliseconds()
			if elapsedMS > int64(o.StallTimeoutMS) {
				stalledIDs = append(stalledIDs, id)
			}
		}
		sort.Strings(stalledIDs)
		for _, id := range stalledIDs {
			entry := o.Running[id]
			if entry == nil {
				continue
			}
			if entry.Cancel != nil {
				entry.Cancel()
			}
			if o.bridge != nil && entry.TaskID != "" {
				_ = o.bridge.StopLinearTask(ctx, entry.TaskID, "stalled: exceeded stall_timeout_ms")
			}
			delete(o.Running, id)
			elapsedSec := now.Sub(entry.StartedAt).Seconds()
			if elapsedSec > 0 {
				o.CodexTotals.SecondsRunning += elapsedSec
			}
			nextAttempt := 1
			if entry.RetryAttempt != nil && *entry.RetryAttempt > 0 {
				nextAttempt = *entry.RetryAttempt + 1
			}
			stallErr := "stalled: exceeded stall_timeout_ms"
			o.LastError[id] = stallErr
			o.appendEventLocked(id, now, "stalled", stallErr)
			o.scheduleRetryLocked(id, entry.Identifier, nextAttempt, BackoffDelayMS(nextAttempt, o.MaxRetryBackoffMS), stallErr)
		}
	}

	runningIDs := make([]string, 0, len(o.Running))
	for id := range o.Running {
		runningIDs = append(runningIDs, id)
	}
	sort.Strings(runningIDs)
	adapter := o.adapter
	o.mu.Unlock()

	if len(runningIDs) == 0 || adapter == nil {
		return
	}

	refreshed, err := adapter.FetchIssuesByIDs(ctx, runningIDs)
	if err != nil {
		o.mu.Lock()
		o.LastTickError = fmt.Sprintf("reconcile tracker refresh failed: %v", err)
		o.mu.Unlock()
		return
	}

	byID := make(map[string]symphony.Issue, len(refreshed))
	for _, iss := range refreshed {
		byID[iss.ID] = iss
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	for _, id := range runningIDs {
		entry, stillRunning := o.Running[id]
		if !stillRunning || entry == nil {
			continue
		}
		iss, found := byID[id]
		if found {
			rec := o.upsertIssueRecordLocked(iss, entry.TaskID, entry.Stage)
			iss = rec.Issue
			entry.Parent = rec.Parent
			entry.Children = rec.Children
		}
		switch {
		case found && symphony.StateInList(iss.State, o.adapter.TerminalStates()):
			o.stopRunningIssueLocked(ctx, id, true, "issue reached terminal state: "+iss.State)
		case found && symphony.StateInList(iss.State, o.adapter.ActiveStates()) && symphony.IssueRoutable(iss, o.RequiredLabels):
			entry.Issue = iss
		default:
			o.stopRunningIssueLocked(ctx, id, false, "issue no longer active or routable")
		}
	}
}

func (o *LinearOrchestrator) stopRunningIssueLocked(ctx context.Context, issueID string, cleanupWorkspace bool, reason string) {
	entry, ok := o.Running[issueID]
	if ok && entry != nil {
		if entry.Cancel != nil {
			entry.Cancel()
		}
		if o.bridge != nil && entry.TaskID != "" {
			_ = o.bridge.StopLinearTask(ctx, entry.TaskID, reason)
		}
		now := o.now().UTC()
		elapsed := now.Sub(entry.StartedAt).Seconds()
		if elapsed > 0 {
			o.CodexTotals.SecondsRunning += elapsed
		}
		if cleanupWorkspace {
			if wm, err := o.workspaceManagerLocked(); err == nil && wm != nil {
				_ = wm.CleanupIssueWorkspace(ctx, entry.Identifier)
			} else {
				_ = RemoveIssueWorkspace(o.WorkspaceRoot, entry.Identifier)
			}
		}
		o.appendEventLocked(issueID, now, "reconcile_stopped", reason)
		o.logStructured("reconcile_stopped", issueID, entry.Identifier, entry.SessionID, fmt.Sprintf("reason=%q", reason))
	}
	delete(o.Running, issueID)
	o.cancelRetryTimerLocked(issueID)
	delete(o.Claimed, issueID)
}

func (o *LinearOrchestrator) selectSSHHostLocked(issueID string) (string, bool) {
	hosts := o.serviceConfig.Worker.SSHHosts
	if len(hosts) == 0 {
		return "", true
	}
	perHostLimit := o.serviceConfig.Worker.MaxConcurrentAgentsPerHost
	if perHostLimit <= 0 {
		perHostLimit = 1
	}
	counts := make(map[string]int, len(hosts))
	for _, entry := range o.Running {
		if entry != nil && entry.SSHHost != "" {
			counts[entry.SSHHost]++
		}
	}
	if pref := strings.TrimSpace(o.LastSSHHost[issueID]); pref != "" {
		for _, h := range hosts {
			if h == pref && counts[h] < perHostLimit {
				return h, true
			}
		}
	}
	bestHost := ""
	bestCount := perHostLimit
	for _, h := range hosts {
		c := counts[h]
		if c < bestCount {
			bestHost = h
			bestCount = c
		}
	}
	if bestHost == "" {
		return "", false
	}
	return bestHost, true
}

func (o *LinearOrchestrator) hasAvailableGlobalSlotLocked() bool {
	return len(o.Running) < o.MaxConcurrentAgents
}

func (o *LinearOrchestrator) availableSlotsForStateLocked(state string) int {
	globalAvail := o.MaxConcurrentAgents - len(o.Running)
	if globalAvail <= 0 {
		return 0
	}
	norm := symphony.NormalizeState(state)
	limit, hasStateLimit := o.MaxConcurrentAgentsByState[norm]
	if !hasStateLimit || limit <= 0 {
		return globalAvail
	}
	runningInState := 0
	for _, entry := range o.Running {
		if symphony.NormalizeState(entry.Issue.State) == norm {
			runningInState++
		}
	}
	stateAvail := limit - runningInState
	if stateAvail <= 0 {
		return 0
	}
	if stateAvail < globalAvail {
		return stateAvail
	}
	return globalAvail
}

func (o *LinearOrchestrator) shouldDispatchLocked(issue symphony.Issue) bool {
	if strings.TrimSpace(issue.ID) == "" ||
		strings.TrimSpace(issue.Identifier) == "" ||
		strings.TrimSpace(issue.Title) == "" ||
		strings.TrimSpace(issue.State) == "" {
		return false
	}
	if o.adapter == nil {
		return false
	}
	if !symphony.StateInList(issue.State, o.adapter.ActiveStates()) {
		return false
	}
	if symphony.StateInList(issue.State, o.adapter.TerminalStates()) {
		return false
	}
	if !symphony.IssueRoutable(issue, o.RequiredLabels) {
		return false
	}
	if _, running := o.Running[issue.ID]; running {
		return false
	}
	if o.Claimed[issue.ID] {
		return false
	}
	if o.availableSlotsForStateLocked(issue.State) <= 0 {
		return false
	}
	if _, ok := o.selectSSHHostLocked(issue.ID); !ok {
		return false
	}
	if !symphony.BlockersCleared(issue, o.adapter.TerminalStates()) {
		return false
	}
	return true
}

// FormatIssueHierarchySummary returns a compact summary of an issue's parent and sub-issues when present.
func FormatIssueHierarchySummary(issue symphony.Issue) string {
	parent, children := tracker.ExtractHierarchy(issue)
	var parts []string
	if parent != nil && parent.Identifier != "" {
		if parent.State != "" {
			parts = append(parts, fmt.Sprintf("parent=%s[%s]", parent.Identifier, parent.State))
		} else {
			parts = append(parts, fmt.Sprintf("parent=%s", parent.Identifier))
		}
	}
	if len(children) > 0 {
		idents := make([]string, 0, len(children))
		for _, ch := range children {
			if ch.Identifier != "" {
				if ch.State != "" {
					idents = append(idents, fmt.Sprintf("%s[%s]", ch.Identifier, ch.State))
				} else {
					idents = append(idents, ch.Identifier)
				}
			} else if ch.ID != "" {
				idents = append(idents, ch.ID)
			}
		}
		if len(idents) > 0 {
			parts = append(parts, "children="+strings.Join(idents, ","))
		}
	}
	return strings.Join(parts, " ")
}

// FormatIssueHierarchyContext returns human-readable parent and sub-issue context lines for prompts.
func FormatIssueHierarchyContext(issue symphony.Issue) string {
	parent, children := tracker.ExtractHierarchy(issue)
	var lines []string
	if parent != nil && (parent.Identifier != "" || parent.ID != "") {
		label := parent.Identifier
		if label == "" {
			label = parent.ID
		}
		line := "Parent Issue: " + label
		if parent.Title != "" {
			line += " (" + parent.Title + ")"
		}
		if parent.State != "" {
			line += " [" + parent.State + "]"
		}
		lines = append(lines, line)
	}
	if len(children) > 0 {
		items := make([]string, 0, len(children))
		for _, ch := range children {
			label := ch.Identifier
			if label == "" {
				label = ch.ID
			}
			item := label
			if ch.Title != "" {
				item += " (" + ch.Title + ")"
			}
			if ch.State != "" {
				item += " [" + ch.State + "]"
			}
			items = append(items, item)
		}
		lines = append(lines, "Sub-Issues: "+strings.Join(items, ", "))
	}
	return strings.Join(lines, "\n")
}

// RenderIssuePrompt renders the workflow prompt template with issue, hierarchy, and attempt fields.
func RenderIssuePrompt(tmpl string, issue symphony.Issue, attempt *int) string {
	hierarchyCtx := FormatIssueHierarchyContext(issue)
	if strings.TrimSpace(tmpl) == "" {
		base := fmt.Sprintf("You are working on Linear issue %s: %s", issue.Identifier, issue.Title)
		if hierarchyCtx != "" {
			return base + "\n" + hierarchyCtx
		}
		return base
	}
	rendered, err := symphony.RenderPrompt(tmpl, issue, attempt)
	if err != nil {
		rendered = strings.TrimSpace(tmpl)
	} else {
		rendered = strings.TrimSpace(rendered)
	}
	if hierarchyCtx != "" && !strings.Contains(tmpl, "issue.parent") && !strings.Contains(tmpl, "issue.children") {
		rendered = rendered + "\n" + hierarchyCtx
	}
	return rendered
}

func (o *LinearOrchestrator) dispatchIssueLocked(ctx context.Context, issue symphony.Issue, attempt *int) error {
	sshHost, ok := o.selectSSHHostLocked(issue.ID)
	if !ok {
		return nil
	}

	var ws symphony.Workspace
	var err error
	wm, wmErr := o.workspaceManagerLocked()
	_, embedded := o.bridge.(linearTaskStateBridge)
	if embedded {
		wm = nil
		ws = symphony.Workspace{WorkspaceKey: symphony.WorkspaceKey(issue.Identifier)}
	} else if wmErr == nil && wm != nil {
		ws, err = wm.CreateForIssue(ctx, issue.Identifier)
	} else {
		ws, err = EnsureIssueWorkspace(o.WorkspaceRoot, issue.Identifier)
	}
	if err != nil {
		o.LastError[issue.ID] = err.Error()
		retryAttempt := 1
		if attempt != nil && *attempt > 0 {
			retryAttempt = *attempt + 1
		}
		o.Claimed[issue.ID] = true
		o.scheduleRetryLocked(issue.ID, issue.Identifier, retryAttempt, BackoffDelayMS(retryAttempt, o.MaxRetryBackoffMS), err.Error())
		o.logStructured("workspace_failed", issue.ID, issue.Identifier, "", fmt.Sprintf("error=%q", err.Error()))
		return err
	}

	var renderedPrompt string
	if strings.TrimSpace(o.promptTemplate) != "" {
		strictRendered, renderErr := symphony.RenderPrompt(o.promptTemplate, issue, attempt)
		if renderErr != nil {
			o.LastError[issue.ID] = renderErr.Error()
			retryAttempt := 1
			if attempt != nil && *attempt > 0 {
				retryAttempt = *attempt + 1
			}
			o.Claimed[issue.ID] = true
			o.scheduleRetryLocked(issue.ID, issue.Identifier, retryAttempt, BackoffDelayMS(retryAttempt, o.MaxRetryBackoffMS), renderErr.Error())
			o.logStructured("prompt_render_failed", issue.ID, issue.Identifier, "", fmt.Sprintf("error=%q", renderErr.Error()))
			return renderErr
		}
		renderedPrompt = strings.TrimSpace(strictRendered)
		hierarchyCtx := FormatIssueHierarchyContext(issue)
		if hierarchyCtx != "" && !strings.Contains(o.promptTemplate, "issue.parent") && !strings.Contains(o.promptTemplate, "issue.children") {
			renderedPrompt = renderedPrompt + "\n" + hierarchyCtx
		}
	} else {
		renderedPrompt = RenderIssuePrompt(o.promptTemplate, issue, attempt)
	}

	if wm != nil {
		if hookErr := wm.RunBeforeRun(ctx, ws.Path); hookErr != nil {
			o.LastError[issue.ID] = hookErr.Error()
			retryAttempt := 1
			if attempt != nil && *attempt > 0 {
				retryAttempt = *attempt + 1
			}
			o.Claimed[issue.ID] = true
			o.scheduleRetryLocked(issue.ID, issue.Identifier, retryAttempt, BackoffDelayMS(retryAttempt, o.MaxRetryBackoffMS), hookErr.Error())
			o.logStructured("before_run_failed", issue.ID, issue.Identifier, "", fmt.Sprintf("error=%q", hookErr.Error()))
			return hookErr
		}
	}

	title := fmt.Sprintf("[%s] %s", issue.Identifier, issue.Title)
	faePriority := LinearPriorityToFae(issue.Priority)

	var taskID string
	if o.bridge != nil {
		admittedID, admitErr := o.bridge.AdmitLinearIssue(ctx, issue, ws.Path, title, renderedPrompt, faePriority)
		if admitErr != nil {
			o.LastError[issue.ID] = admitErr.Error()
			retryAttempt := 1
			if attempt != nil && *attempt > 0 {
				retryAttempt = *attempt + 1
			}
			o.Claimed[issue.ID] = true
			o.scheduleRetryLocked(issue.ID, issue.Identifier, retryAttempt, BackoffDelayMS(retryAttempt, o.MaxRetryBackoffMS), admitErr.Error())
			return admitErr
		}
		taskID = admittedID
		if stateBridge, ok := o.bridge.(linearTaskStateBridge); ok {
			if status, found, lookupErr := stateBridge.LookupLinearTask(ctx, issue.ID); lookupErr == nil && found {
				ws.Path = status.WorkspacePath
				if status.Terminal || status.NeedsOperator {
					o.TaskToIssue[taskID] = issue.ID
					o.IssueToTask[issue.ID] = taskID
					o.Claimed[issue.ID] = true
					o.Completed[issue.ID] = status.Completed
					o.cancelRetryTimerLocked(issue.ID)
					o.upsertIssueRecordLocked(issue, taskID, status.State)
					return nil
				}
			}
		}
		o.TaskToIssue[taskID] = issue.ID
		o.IssueToTask[issue.ID] = taskID
	}

	if o.transitionStateOnDispatch != "" && o.adapter != nil {
		if symphony.NormalizeState(issue.State) != symphony.NormalizeState(o.transitionStateOnDispatch) {
			if updated, transErr := o.adapter.TransitionIssueState(ctx, issue.ID, o.transitionStateOnDispatch); transErr == nil && updated.State != "" {
				issue = updated
			}
		}
	}
	rec := o.upsertIssueRecordLocked(issue, taskID, "implement")
	issue = rec.Issue
	hierarchySummary := FormatIssueHierarchySummary(issue)
	if o.postStartComment && o.adapter != nil {
		commentBody := fmt.Sprintf("Started work on `%s` in workspace `%s`", issue.Identifier, ws.WorkspaceKey)
		if taskID != "" {
			commentBody = fmt.Sprintf("Admitted Fae task `%s` for `%s` (priority %d) in workspace `%s`.", taskID, issue.Identifier, faePriority, ws.WorkspaceKey)
		}
		if hierarchySummary != "" {
			commentBody += " (" + hierarchySummary + ")"
		}
		_, _ = o.adapter.AddIssueComment(ctx, issue.ID, commentBody)
	}

	now := o.now().UTC()
	workerCtx, cancel := context.WithCancel(ctx)
	var attemptCopy *int
	if attempt != nil {
		val := *attempt
		attemptCopy = &val
		o.RestartCounts[issue.ID] = val
	}
	if sshHost != "" {
		o.LastSSHHost[issue.ID] = sshHost
	}
	entry := &RunningIssueEntry{
		Issue:         issue,
		IssueID:       issue.ID,
		Identifier:    issue.Identifier,
		TaskID:        taskID,
		Stage:         "implement",
		Parent:        rec.Parent,
		Children:      rec.Children,
		WorkspacePath: ws.Path,
		SSHHost:       sshHost,
		RetryAttempt:  attemptCopy,
		StartedAt:     now,
		LastMessage:   hierarchySummary,
		Cancel:        cancel,
	}
	o.Running[issue.ID] = entry
	o.Claimed[issue.ID] = true
	o.cancelRetryTimerLocked(issue.ID)
	delete(o.LastError, issue.ID)
	dispatchMsg := fmt.Sprintf("dispatched %s (task=%s)", issue.Identifier, taskID)
	if hierarchySummary != "" {
		dispatchMsg += " " + hierarchySummary
	}
	o.appendEventLocked(issue.ID, now, "dispatched", dispatchMsg)
	o.logStructured("dispatched", issue.ID, issue.Identifier, "", fmt.Sprintf("workspace=%q ssh_host=%q", ws.Path, sshHost))

	if embedded {
		return nil
	}
	launcher := o.workerLauncher
	if launcher == nil && o.enableAppServerRunner {
		hostCopy := sshHost
		launcher = func(wctx context.Context, iss symphony.Issue, w symphony.Workspace, p string, att *int) error {
			return o.RunCodingAgentSession(wctx, iss, w, hostCopy, p, att)
		}
	}
	if launcher != nil {
		issCopy := issue
		wsCopy := ws
		promptCopy := renderedPrompt
		attCopy := attemptCopy
		go func() {
			runErr := launcher(workerCtx, issCopy, wsCopy, promptCopy, attCopy)
			if workerCtx.Err() != nil {
				return
			}
			o.OnWorkerExit(issCopy.ID, runErr == nil, runErr)
		}()
	}
	return nil
}

// RunCodingAgentSession runs the multi-turn JSON-RPC coding-agent app-server client for issue in ws (Symphony §10).
func (o *LinearOrchestrator) RunCodingAgentSession(ctx context.Context, issue symphony.Issue, ws symphony.Workspace, sshHost string, prompt string, attempt *int) error {
	o.mu.RLock()
	wsRoot := o.WorkspaceRoot
	codexCfg := o.serviceConfig.Codex
	maxTurns := o.serviceConfig.Agent.MaxTurns
	adapter := o.adapter
	reqLabels := append([]string(nil), o.RequiredLabels...)
	o.mu.RUnlock()

	var toolSpecs []symphony.RunnerToolSpec
	if adapter != nil {
		for _, spec := range adapter.AgentToolSpecs() {
			toolSpecs = append(toolSpecs, symphony.RunnerToolSpec{
				Name:        spec.Name,
				Description: spec.Description,
				InputSchema: spec.InputSchema,
			})
		}
	}

	runner := &symphony.AppServerRunner{
		WorkspaceRoot: wsRoot,
		Codex:         codexCfg,
		MaxTurns:      maxTurns,
		SSHHost:       sshHost,
		Env:           o.StrippedAgentEnv(os.Environ()),
		DynamicTools:  toolSpecs,
		Logger:        o.logf,
		ExecuteTool: func(tctx context.Context, name string, arguments map[string]any) (symphony.RunnerToolResult, bool) {
			res := o.ExecuteAgentTool(tctx, issue.ID, name, arguments)
			if !res.Success && strings.Contains(strings.ToLower(res.Error), "unsupported") {
				return symphony.RunnerToolResult{}, false
			}
			return symphony.RunnerToolResult{
				Success: res.Success,
				Output:  res.Output,
				Error:   res.Error,
			}, true
		},
		OnEvent: func(ev symphony.RunnerEvent) {
			var usage *TokenUsagePayload
			if ev.HasTokenUsage {
				usage = &TokenUsagePayload{
					TotalTokenUsage: &TokenCounts{
						InputTokens:  ev.InputTokens,
						OutputTokens: ev.OutputTokens,
						TotalTokens:  ev.TotalTokens,
					},
				}
			}
			o.RecordAgentEvent(issue.ID, AgentEvent{
				Event:             ev.Event,
				Timestamp:         ev.Timestamp,
				Message:           ev.Message,
				CodexAppServerPID: ev.CodexAppServerPID,
				SessionID:         ev.SessionID,
				ThreadID:          ev.ThreadID,
				TurnID:            ev.TurnID,
				Usage:             usage,
				RateLimits:        ev.RateLimits,
				Raw:               ev.Raw,
			})
		},
		CheckActive: func(cctx context.Context, current symphony.Issue) (symphony.Issue, bool, error) {
			if adapter == nil {
				return current, false, nil
			}
			refreshedList, err := adapter.FetchIssuesByIDs(cctx, []string{current.ID})
			if err != nil {
				return current, false, err
			}
			if len(refreshedList) == 0 {
				return current, false, nil
			}
			refreshed := refreshedList[0]
			stillActive := symphony.StateInList(refreshed.State, adapter.ActiveStates()) &&
				!symphony.StateInList(refreshed.State, adapter.TerminalStates()) &&
				symphony.IssueRoutable(refreshed, reqLabels)
			return refreshed, stillActive, nil
		},
	}

	return runner.Run(ctx, issue, ws, prompt, attempt)
}

// OnWorkerExit handles normal or abnormal coding-agent / worker completion (Symphony §7.1, §8.4, §16.6).
func (o *LinearOrchestrator) OnWorkerExit(issueID string, normal bool, exitErr error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	now := o.now().UTC()
	entry, wasRunning := o.Running[issueID]
	identifier := issueID
	sessionID := ""
	wsPath := ""
	var prevAttempt *int
	if wasRunning && entry != nil {
		identifier = entry.Identifier
		sessionID = entry.SessionID
		wsPath = entry.WorkspacePath
		prevAttempt = entry.RetryAttempt
		if entry.Cancel != nil {
			entry.Cancel()
		}
		elapsed := now.Sub(entry.StartedAt).Seconds()
		if elapsed > 0 {
			o.CodexTotals.SecondsRunning += elapsed
		}
		delete(o.Running, issueID)
	} else if known, ok := o.KnownIssues[issueID]; ok {
		identifier = known.Identifier
	}
	if wsPath != "" {
		if wm, err := o.workspaceManagerLocked(); err == nil && wm != nil {
			_ = wm.RunAfterRun(context.Background(), wsPath)
		}
	}

	o.Claimed[issueID] = true
	if normal {
		o.Completed[issueID] = true
		o.appendEventLocked(issueID, now, "worker_exit_normal", "worker exited normally; scheduling continuation check")
		o.logStructured("worker_exit_normal", issueID, identifier, sessionID)
		o.scheduleRetryLocked(issueID, identifier, 1, continuationRetryDelayMS, "")
		return
	}

	nextAttempt := 1
	if prevAttempt != nil && *prevAttempt > 0 {
		nextAttempt = *prevAttempt + 1
	}
	errMsg := "worker exited abnormally"
	if exitErr != nil {
		errMsg = exitErr.Error()
	}
	o.LastError[issueID] = errMsg
	o.appendEventLocked(issueID, now, "worker_exit_failed", errMsg)
	o.logStructured("worker_exit_failed", issueID, identifier, sessionID, fmt.Sprintf("error=%q attempt=%d", errMsg, nextAttempt))
	delayMS := BackoffDelayMS(nextAttempt, o.MaxRetryBackoffMS)
	o.scheduleRetryLocked(issueID, identifier, nextAttempt, delayMS, errMsg)
}

func (o *LinearOrchestrator) cancelRetryTimerLocked(issueID string) {
	if existing, ok := o.RetryAttempts[issueID]; ok && existing != nil {
		if existing.Timer != nil {
			existing.Timer.Stop()
		}
		delete(o.RetryAttempts, issueID)
	}
}

func (o *LinearOrchestrator) scheduleRetryLocked(issueID string, identifier string, attempt int, delayMS int, errMsg string) {
	o.cancelRetryTimerLocked(issueID)
	now := o.now().UTC()
	dueAt := now.Add(time.Duration(delayMS) * time.Millisecond)
	entry := &RetryEntry{
		IssueID:    issueID,
		Identifier: identifier,
		Attempt:    attempt,
		DueAt:      dueAt,
		DueAtMS:    dueAt.UnixMilli(),
		Error:      errMsg,
	}
	if o.enableRetryTimers {
		idCopy := issueID
		retryContext := o.loopContext
		if retryContext == nil {
			retryContext = context.Background()
		}
		entry.Timer = time.AfterFunc(time.Duration(delayMS)*time.Millisecond, func() {
			if retryContext.Err() == nil {
				_ = o.OnRetryTimer(retryContext, idCopy)
			}
		})
	}
	o.RetryAttempts[issueID] = entry
}

// OnRetryTimer handles a fired retry timer for issueID (Symphony §8.4, §16.6).
func (o *LinearOrchestrator) OnRetryTimer(ctx context.Context, issueID string) error {
	o.mu.Lock()
	retryEntry, ok := o.RetryAttempts[issueID]
	if !ok || retryEntry == nil {
		o.mu.Unlock()
		return nil
	}
	if retryEntry.Timer != nil {
		retryEntry.Timer.Stop()
	}
	delete(o.RetryAttempts, issueID)
	adapter := o.adapter
	o.mu.Unlock()

	if adapter == nil {
		o.mu.Lock()
		delete(o.Claimed, issueID)
		o.mu.Unlock()
		return nil
	}

	issues, err := adapter.FetchIssuesByIDs(ctx, []string{issueID})
	if err != nil {
		o.mu.Lock()
		nextAttempt := retryEntry.Attempt + 1
		errMsg := "retry poll failed: " + err.Error()
		o.LastError[issueID] = errMsg
		o.scheduleRetryLocked(issueID, retryEntry.Identifier, nextAttempt, BackoffDelayMS(nextAttempt, o.MaxRetryBackoffMS), errMsg)
		o.mu.Unlock()
		return err
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	if len(issues) == 0 {
		delete(o.Claimed, issueID)
		return nil
	}
	issue := issues[0]
	rec := o.upsertIssueRecordLocked(issue, "", "")
	issue = rec.Issue

	if symphony.StateInList(issue.State, o.adapter.TerminalStates()) {
		if wm, err := o.workspaceManagerLocked(); err == nil && wm != nil {
			_ = wm.CleanupIssueWorkspace(ctx, issue.Identifier)
		} else {
			_ = RemoveIssueWorkspace(o.WorkspaceRoot, issue.Identifier)
		}
		delete(o.Claimed, issueID)
		return nil
	}

	if !symphony.StateInList(issue.State, o.adapter.ActiveStates()) || !symphony.IssueRoutable(issue, o.RequiredLabels) {
		delete(o.Claimed, issueID)
		return nil
	}

	if !symphony.BlockersCleared(issue, o.adapter.TerminalStates()) {
		delete(o.Claimed, issueID)
		return nil
	}

	if o.availableSlotsForStateLocked(issue.State) <= 0 {
		nextAttempt := retryEntry.Attempt + 1
		slotErr := "no available orchestrator slots"
		o.LastError[issueID] = slotErr
		o.scheduleRetryLocked(issueID, issue.Identifier, nextAttempt, BackoffDelayMS(nextAttempt, o.MaxRetryBackoffMS), slotErr)
		return nil
	}

	if _, ok := o.selectSSHHostLocked(issue.ID); !ok {
		nextAttempt := retryEntry.Attempt + 1
		slotErr := "no available SSH host slots"
		o.LastError[issueID] = slotErr
		o.scheduleRetryLocked(issueID, issue.Identifier, nextAttempt, BackoffDelayMS(nextAttempt, o.MaxRetryBackoffMS), slotErr)
		return nil
	}

	attempt := retryEntry.Attempt
	return o.dispatchIssueLocked(ctx, issue, &attempt)
}

// RecordAgentEvent updates live session metadata, token deltas, and rate limits (Symphony §13.5).
// It extracts token usage from cumulative total_token_usage and ignores delta-only last_token_usage
// payloads to avoid double-counting.
func (o *LinearOrchestrator) RecordAgentEvent(issueID string, ev AgentEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()

	ts := ev.Timestamp
	if ts.IsZero() {
		ts = o.now().UTC()
	}
	if ev.RateLimits != nil {
		o.RateLimits = ev.RateLimits
	}
	o.appendEventLocked(issueID, ts, ev.Event, ev.Message)

	entry, ok := o.Running[issueID]
	if !ok || entry == nil {
		return
	}
	entry.LastEvent = ev.Event
	if ev.Message != "" {
		entry.LastMessage = ev.Message
	}
	entry.LastEventAt = &ts
	if ev.SessionID != "" {
		entry.SessionID = ev.SessionID
	}
	if ev.ThreadID != "" {
		entry.ThreadID = ev.ThreadID
	}
	if ev.TurnID != "" {
		entry.TurnID = ev.TurnID
	}
	if entry.SessionID == "" && entry.ThreadID != "" && entry.TurnID != "" {
		entry.SessionID = entry.ThreadID + "-" + entry.TurnID
	}
	if ev.CodexAppServerPID != "" {
		entry.CodexAppServerPID = ev.CodexAppServerPID
	}
	switch ev.Event {
	case "turn_started", "turn/started":
		entry.TurnCount++
	case "session_started":
		if entry.TurnCount == 0 {
			entry.TurnCount = 1
		}
	}
	o.logStructured(ev.Event, issueID, entry.Identifier, entry.SessionID)

	if counts, hasCumulative := extractCumulativeTokens(ev); hasCumulative {
		inDelta := counts.InputTokens - entry.LastReportedInputTokens
		if inDelta < 0 {
			inDelta = 0
		}
		outDelta := counts.OutputTokens - entry.LastReportedOutputTokens
		if outDelta < 0 {
			outDelta = 0
		}
		totDelta := counts.TotalTokens - entry.LastReportedTotalTokens
		if totDelta < 0 {
			totDelta = 0
		}

		entry.InputTokens += inDelta
		entry.OutputTokens += outDelta
		entry.TotalTokens += totDelta
		if counts.InputTokens > entry.LastReportedInputTokens {
			entry.LastReportedInputTokens = counts.InputTokens
		}
		if counts.OutputTokens > entry.LastReportedOutputTokens {
			entry.LastReportedOutputTokens = counts.OutputTokens
		}
		if counts.TotalTokens > entry.LastReportedTotalTokens {
			entry.LastReportedTotalTokens = counts.TotalTokens
		}

		o.CodexTotals.InputTokens += inDelta
		o.CodexTotals.OutputTokens += outDelta
		o.CodexTotals.TotalTokens += totDelta
	}
}

func extractCumulativeTokens(ev AgentEvent) (TokenCounts, bool) {
	if ev.Usage != nil {
		if ev.Usage.TotalTokenUsage != nil {
			t := *ev.Usage.TotalTokenUsage
			if t.TotalTokens == 0 && (t.InputTokens > 0 || t.OutputTokens > 0) {
				t.TotalTokens = t.InputTokens + t.OutputTokens
			}
			return t, true
		}
		// Ignore delta-only last_token_usage payloads to prevent double-counting (Symphony §13.5).
		if ev.Usage.LastTokenUsage != nil && ev.Usage.InputTokens == 0 && ev.Usage.OutputTokens == 0 && ev.Usage.TotalTokens == 0 {
			return TokenCounts{}, false
		}
		if ev.Usage.InputTokens > 0 || ev.Usage.OutputTokens > 0 || ev.Usage.TotalTokens > 0 {
			tot := ev.Usage.TotalTokens
			if tot == 0 {
				tot = ev.Usage.InputTokens + ev.Usage.OutputTokens
			}
			return TokenCounts{
				InputTokens:  ev.Usage.InputTokens,
				OutputTokens: ev.Usage.OutputTokens,
				TotalTokens:  tot,
			}, true
		}
	}
	if ev.Raw != nil {
		if totalMap, ok := ev.Raw["total_token_usage"].(map[string]any); ok {
			return mapToTokenCounts(totalMap), true
		}
		if usageMap, ok := ev.Raw["usage"].(map[string]any); ok {
			if totalMap, ok := usageMap["total_token_usage"].(map[string]any); ok {
				return mapToTokenCounts(totalMap), true
			}
		}
	}
	return TokenCounts{}, false
}

func mapToTokenCounts(m map[string]any) TokenCounts {
	in := toInt64(m["input_tokens"])
	out := toInt64(m["output_tokens"])
	tot := toInt64(m["total_tokens"])
	if tot == 0 {
		tot = in + out
	}
	return TokenCounts{
		InputTokens:  in,
		OutputTokens: out,
		TotalTokens:  tot,
	}
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

func (o *LinearOrchestrator) appendEventLocked(issueID string, at time.Time, event string, message string) {
	list := append(o.RecentEvents[issueID], RecentEventEntry{
		At:      at,
		Event:   event,
		Message: message,
	})
	if len(list) > maxRecentEventsPerIssue {
		list = list[len(list)-maxRecentEventsPerIssue:]
	}
	o.RecentEvents[issueID] = list
}

// MapFaeStageToLinearState maps a Fae workflow stage and task state to a Linear workflow state name.
func MapFaeStageToLinearState(stage string, taskState string) string {
	normState := symphony.NormalizeState(taskState)
	normStage := symphony.NormalizeState(stage)

	switch normState {
	case "done":
		return "Done"
	case "cancelled", "canceled", "retired":
		return "Canceled"
	case "escalated", "human review":
		return "In Review"
	}

	switch normStage {
	case "review", "human_review", "in_review":
		return "In Review"
	case "land":
		if normState == "done" {
			return "Done"
		}
		return "In Review"
	case "implement", "verify", "plan", "execute":
		return "In Progress"
	default:
		if normState == "running" || normState == "pending" {
			return "In Progress"
		}
		return "In Progress"
	}
}

// SyncTaskStageToLinear updates the corresponding Linear issue's workflow state and posts a progress comment
// as a Fae task progresses through workflow stages (implement, verify, review, land) or finishes.
func (o *LinearOrchestrator) SyncTaskStageToLinear(ctx context.Context, taskID string, stage string, state string, summary string) error {
	o.mu.Lock()
	issueID, ok := o.TaskToIssue[taskID]
	if !ok {
		if _, known := o.KnownIssues[taskID]; known {
			issueID = taskID
			ok = true
		} else {
			for id, iss := range o.KnownIssues {
				if strings.EqualFold(iss.Identifier, taskID) {
					issueID = id
					ok = true
					break
				}
			}
		}
	}
	adapter := o.adapter
	o.mu.Unlock()

	if !ok || issueID == "" {
		return fmt.Errorf("unknown task or issue mapping for %q", taskID)
	}
	if adapter == nil {
		return errors.New("missing tracker adapter")
	}

	targetLinearState := MapFaeStageToLinearState(stage, state)
	updatedIssue, err := adapter.TransitionIssueState(ctx, issueID, targetLinearState)
	if err != nil {
		return err
	}

	commentBody := fmt.Sprintf("Fae task `%s` stage `%s` (%s)", taskID, stage, state)
	if strings.TrimSpace(summary) != "" {
		commentBody = fmt.Sprintf("Fae task `%s` stage `%s` (%s): %s", taskID, stage, state, summary)
	}
	commentID, err := adapter.AddIssueComment(ctx, issueID, commentBody)
	if err != nil {
		return err
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now().UTC()
	if updatedIssue.ID != "" {
		rec := o.upsertIssueRecordLocked(updatedIssue, taskID, stage)
		updatedIssue = rec.Issue
	} else if prev, exists := o.KnownIssues[issueID]; exists {
		prev.State = targetLinearState
		o.upsertIssueRecordLocked(prev, taskID, stage)
	}
	if entry, running := o.Running[issueID]; running && entry != nil {
		entry.Stage = stage
		entry.Issue.State = targetLinearState
		entry.LastEvent = "stage:" + stage
		entry.LastMessage = summary
		entry.LastEventAt = &now
	}
	o.StageHistory[issueID] = append(o.StageHistory[issueID], StageTransitionRecord{
		TaskID:      taskID,
		IssueID:     issueID,
		Stage:       stage,
		TaskState:   state,
		LinearState: targetLinearState,
		Summary:     summary,
		CommentID:   commentID,
		At:          now,
	})
	o.appendEventLocked(issueID, now, "stage:"+stage, commentBody)
	return nil
}

// ExecuteAgentTool executes a host-side provider tool (e.g., linear_get_issue, linear_update_issue_state,
// linear_create_comment, linear_add_attachment_link) on behalf of a coding agent working on issueID.
func (o *LinearOrchestrator) ExecuteAgentTool(ctx context.Context, issueID string, toolName string, args map[string]any) tracker.ToolResult {
	o.mu.RLock()
	adapter := o.adapter
	var issue symphony.Issue
	var found bool
	if entry, ok := o.Running[issueID]; ok && entry != nil {
		issue = entry.Issue
		found = true
	} else if known, ok := o.KnownIssues[issueID]; ok {
		issue = known
		found = true
	} else if mappedID, ok := o.TaskToIssue[issueID]; ok {
		if known, ok2 := o.KnownIssues[mappedID]; ok2 {
			issue = known
			found = true
		}
	} else {
		for _, known := range o.KnownIssues {
			if strings.EqualFold(known.Identifier, issueID) {
				issue = known
				found = true
				break
			}
		}
	}
	o.mu.RUnlock()

	if adapter == nil {
		return tracker.ToolResult{Success: false, Error: "tracker adapter not configured"}
	}
	if !found {
		return tracker.ToolResult{Success: false, Error: fmt.Sprintf("issue %q not found", issueID)}
	}
	res := adapter.ExecuteAgentTool(ctx, toolName, args, tracker.ToolContext{Issue: issue})
	if res.Success && toolName == "linear_update_issue_state" {
		if newState, ok := res.Output["state"].(string); ok && newState != "" {
			o.mu.Lock()
			issue.State = newState
			o.KnownIssues[issue.ID] = issue
			if entry, running := o.Running[issue.ID]; running && entry != nil {
				entry.Issue.State = newState
			}
			o.mu.Unlock()
		}
	}
	return res
}

// StrippedAgentEnv returns a copy of baseEnv with all tracker secret environment variables removed (Symphony §10.5).
func (o *LinearOrchestrator) StrippedAgentEnv(baseEnv []string) []string {
	o.mu.RLock()
	adapter := o.adapter
	o.mu.RUnlock()
	if adapter == nil {
		return append([]string(nil), baseEnv...)
	}
	secrets := make(map[string]bool)
	for _, name := range adapter.SecretEnvironmentNames() {
		trimmed := strings.TrimSpace(name)
		if trimmed != "" {
			secrets[trimmed] = true
		}
	}
	if len(secrets) == 0 {
		return append([]string(nil), baseEnv...)
	}
	out := make([]string, 0, len(baseEnv))
	for _, kv := range baseEnv {
		k, _, _ := strings.Cut(kv, "=")
		if !secrets[k] {
			out = append(out, kv)
		}
	}
	return out
}

// TriggerRefresh queues or coalesces an immediate poll + reconciliation cycle (Symphony §13.7.2).
func (o *LinearOrchestrator) TriggerRefresh(ctx context.Context) (queued bool, coalesced bool) {
	o.refreshMu.Lock()
	if o.refreshInProgress {
		o.refreshPending = true
		o.refreshMu.Unlock()
		return true, true
	}
	o.refreshInProgress = true
	o.refreshMu.Unlock()

	go func() {
		for {
			_ = o.Tick(ctx)
			o.refreshMu.Lock()
			if o.refreshPending {
				o.refreshPending = false
				o.refreshMu.Unlock()
				continue
			}
			o.refreshInProgress = false
			o.refreshMu.Unlock()
			return
		}
	}()
	return true, false
}

// RunLoop executes preflight validation, StartupCleanup, and then runs the periodic Tick loop until ctx is cancelled (Symphony §8.1).
func (o *LinearOrchestrator) RunLoop(ctx context.Context, once bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	o.mu.Lock()
	o.loopContext = ctx
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		for id := range o.RetryAttempts {
			o.cancelRetryTimerLocked(id)
		}
		for _, entry := range o.Running {
			if entry != nil && entry.Cancel != nil {
				entry.Cancel()
			}
		}
	}()

	_ = o.reloadConfigIfChanged()
	if err := o.ValidateDispatchConfig(); err != nil {
		o.mu.Lock()
		o.LastTickError = err.Error()
		o.mu.Unlock()
		return err
	}
	o.StartupCleanup(ctx)
	if err := o.Tick(ctx); err != nil && once {
		return err
	}
	if once {
		return nil
	}
	o.mu.RLock()
	intervalMS := o.PollIntervalMS
	o.mu.RUnlock()
	if intervalMS <= 0 {
		intervalMS = defaultPollIntervalMS
	}
	ticker := time.NewTicker(time.Duration(intervalMS) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = o.Tick(ctx)
			o.mu.RLock()
			newIntervalMS := o.PollIntervalMS
			o.mu.RUnlock()
			if newIntervalMS > 0 && newIntervalMS != intervalMS {
				intervalMS = newIntervalMS
				ticker.Reset(time.Duration(intervalMS) * time.Millisecond)
			}
		}
	}
}

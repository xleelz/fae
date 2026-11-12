package client

import (
	"encoding/json"
	"time"
)

// TurnBudgetView represents a role or task turn budget (FAE-RT-015, FAE-OPS-034).
type TurnBudgetView struct {
	WallSeconds       int    `json:"wall_seconds,omitempty"`
	MaxAssistantTurns int    `json:"max_assistant_turns,omitempty"`
	MaxTurns          int    `json:"max_turns,omitempty"`
	OnExhaustion      string `json:"on_exhaustion,omitempty"`
}

// ReadPolicyView represents a task or role read policy (FAE-SEC-008).
type ReadPolicyView struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// NotificationView represents a task notification (FAE-OPS-033).
type NotificationView struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Delivery    string    `json:"delivery"`
	Summary     string    `json:"summary"`
	FromTask    string    `json:"from_task,omitempty"`
	Actor       string    `json:"actor,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	DeliveredAt time.Time `json:"delivered_at,omitempty"`
}

// TurnCheckpointView represents a durable agentic turn checkpoint (FAE-RT-019).
type TurnCheckpointView struct {
	ProviderSession string    `json:"provider_session"`
	WorktreeTree    string    `json:"worktree_tree,omitempty"`
	Stage           string    `json:"stage,omitempty"`
	AssistantTurns  int       `json:"assistant_turns,omitempty"`
	WallSeconds     int       `json:"wall_seconds,omitempty"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
}

// FanBranchView represents a single branch inside an active FanOut (FAE-WF-096).
type FanBranchView struct {
	ID         string   `json:"id"`
	Status     string   `json:"status"`
	Stage      string   `json:"stage,omitempty"`
	InFlight   bool     `json:"in_flight,omitempty"`
	WriteScope []string `json:"write_scope,omitempty"`
	CommitSHA  string   `json:"commit_sha,omitempty"`
	BranchRef  string   `json:"branch_ref,omitempty"`
	Tree       string   `json:"tree,omitempty"`
	BaseTree   string   `json:"base_tree,omitempty"`
	Item       string   `json:"item,omitempty"`
}

// FanView represents an active intra-task FanOut state (FAE-WF-096).
type FanView struct {
	Stage    string          `json:"stage"`
	Join     string          `json:"join,omitempty"`
	Parallel bool            `json:"parallel,omitempty"`
	Branches []FanBranchView `json:"branches,omitempty"`
}

// PendingRequestView represents an active scope-widen or red-rework request.
type PendingRequestView struct {
	Kind     string   `json:"kind"`
	Paths    []string `json:"paths,omitempty"`
	Files    []string `json:"files,omitempty"`
	TestFile string   `json:"test_file,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

// ProviderPoolView represents a provider concurrency pool (FAE-SCH-015, FAE-RT-018).
type ProviderPoolView struct {
	ID                   string    `json:"id"`
	Provider             string    `json:"provider"`
	CredentialIdentity   string    `json:"credential_identity"`
	Capacity             int       `json:"capacity"`
	AdmissionLimit       int       `json:"admission_limit"`
	Holders              []string  `json:"holders,omitempty"`
	Waiting              int       `json:"waiting"`
	Paused               bool      `json:"paused"`
	PauseActor           string    `json:"pause_actor,omitempty"`
	PauseReason          string    `json:"pause_reason,omitempty"`
	QuotaResetAt         time.Time `json:"quota_reset_at,omitempty"`
	QuotaExhaustedUntil  time.Time `json:"quota_exhausted_until,omitempty"`
	CutOffTaskID         string    `json:"cut_off_task_id,omitempty"`
	QuotaReason          string    `json:"quota_reason,omitempty"`
}

// ProviderBindingHealthView represents a provider binding health and quota/backoff status (FAE-RT-018).
type ProviderBindingHealthView struct {
	Provider            string    `json:"provider"`
	Model               string    `json:"model"`
	BindingID           string    `json:"binding_id,omitempty"`
	Score               float64   `json:"score"`
	EffectiveScore      float64   `json:"effective_score"`
	Eligible            bool      `json:"eligible"`
	Outcome             string    `json:"outcome,omitempty"`
	Failures            int       `json:"failures"`
	QuotaResetAt        time.Time `json:"quota_reset_at,omitempty"`
	QuotaExhaustedUntil time.Time `json:"quota_exhausted_until,omitempty"`
	CutOffTaskID        string    `json:"cut_off_task_id,omitempty"`
	RateLimitProbes     int       `json:"rate_limit_probes"`
	NextProbeAt         time.Time `json:"next_probe_at,omitempty"`
}

// DispatchPauseView represents a scoped dispatch pause (FAE-RT-018).
type DispatchPauseView struct {
	Scope     string    `json:"scope"`
	Target    string    `json:"target"`
	Paused    bool      `json:"paused"`
	Actor     string    `json:"actor,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// PaneSessionView represents an active agent execution pane or session.
type PaneSessionView struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	Role      string    `json:"role"`
	Stage     string    `json:"stage,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
}

// SessionAnalysisView represents AnalyzeSession metrics for a task.
type SessionAnalysisView struct {
	TaskID           string         `json:"task_id"`
	Turns            int            `json:"turns"`
	WallSeconds      float64        `json:"wall_seconds"`
	Retries          int            `json:"retries"`
	Parks            int            `json:"parks"`
	LeaseWaitSeconds float64        `json:"lease_wait_seconds"`
	TurnsPerRole     map[string]int `json:"turns_per_role,omitempty"`
}

// ReadRootView represents an external read root configuration (FAE-RT-016).
type ReadRootView struct {
	Name   string   `json:"name"`
	Path   string   `json:"path"`
	Commit string   `json:"commit,omitempty"`
	Digest string   `json:"digest,omitempty"`
	Deny   []string `json:"deny,omitempty"`
}

// TargetConfigView represents a target configuration policy (FAE-RT-016, FAE-SEC-009, FAE-CTL-052, FAE-EXT-023).
type TargetConfigView struct {
	ID             string                  `json:"id"`
	Revision       int64                   `json:"revision"`
	ReadRoots      map[string]ReadRootView `json:"read_roots,omitempty"`
	DenyPaths      []string                `json:"deny_paths,omitempty"`
	DocumentGlobs  []string                `json:"document_globs,omitempty"`
	DocumentChecks []string                `json:"document_checks,omitempty"`
	CommitName     string                  `json:"commit_name,omitempty"`
	CommitEmail    string                  `json:"commit_email,omitempty"`
	PushMode       string                  `json:"push_mode,omitempty"`
	PushRemote     string                  `json:"push_remote,omitempty"`
	PushBranch     string                  `json:"push_branch,omitempty"`
}

// EventView represents an operator/security/sync event from Events RPC.
type EventView struct {
	ID      int64           `json:"id"`
	Kind    string          `json:"kind"`
	Task    string          `json:"task,omitempty"`
	Actor   string          `json:"actor,omitempty"`
	At      time.Time       `json:"created_at"`
	Summary string          `json:"summary,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ProvenanceEntryView represents a turn provenance entry (FAE-RT-016, FAE-SEC-008).
type ProvenanceEntryView struct {
	DispatchID string            `json:"dispatch_id"`
	Stage      string            `json:"stage"`
	Role       string            `json:"role,omitempty"`
	Outcome    string            `json:"outcome,omitempty"`
	CommitSHA  string            `json:"commit_sha,omitempty"`
	ReadRoots  map[string]string `json:"read_roots,omitempty"`
	Reads      *ReadPolicyView   `json:"reads,omitempty"`
	CreatedAt  time.Time         `json:"created_at,omitempty"`
}

// LandStatusView represents a task's landing status.
type LandStatusView struct {
	TaskID     string `json:"task_id"`
	Landed     bool   `json:"landed"`
	TargetHead string `json:"target_head,omitempty"`
	TaskCommit string `json:"task_commit,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// LinearIssueView represents a Symphony / Linear issue synced with Fae.
type LinearIssueView struct {
	ID           string    `json:"id"`
	Identifier   string    `json:"identifier"`
	Title        string    `json:"title"`
	State        string    `json:"state"`
	Priority     int       `json:"priority"`
	Dispatchable bool      `json:"dispatchable"`
	TaskID       string    `json:"task_id,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`
	Stage        string    `json:"stage,omitempty"`
	TurnCount    int       `json:"turn_count"`
	TokensTotal  int       `json:"tokens_total"`
	BlockedBy    []string  `json:"blocked_by,omitempty"`
	SubIssues    []string  `json:"sub_issues,omitempty"`
	URL          string    `json:"url,omitempty"`
	RetryAttempt int       `json:"retry_attempt,omitempty"`
	RetryDueAt   time.Time `json:"retry_due_at,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
}

// TicketOutboxView represents tracker outbox health in Status.
type TicketOutboxView struct {
	Pending          int       `json:"pending"`
	OldestAgeSeconds float64   `json:"oldest_age_seconds"`
	MaxAttempts      int       `json:"max_attempts"`
	LastError        string    `json:"last_error,omitempty"`
	CircuitOpenUntil time.Time `json:"circuit_open_until,omitempty"`
}

// SymphonyStateView aggregates Symphony / Linear tracker state.
type SymphonyStateView struct {
	Running        []LinearIssueView `json:"running,omitempty"`
	Retrying       []LinearIssueView `json:"retrying,omitempty"`
	KnownIssues    []LinearIssueView `json:"known_issues,omitempty"`
	Outbox         TicketOutboxView  `json:"outbox"`
	TotalTokens    int               `json:"total_tokens"`
	PollIntervalMs int               `json:"poll_interval_ms"`
}

// NotifyParams is Notify RPC input (FAE-OPS-033).
type NotifyParams struct {
	Task               string `json:"task"`
	Kind               string `json:"kind"`
	Delivery           string `json:"delivery"`
	Summary            string `json:"summary"`
	FromTask           string `json:"from_task,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}

// FollowUpParams is FollowUp RPC input (FAE-OPS-034).
type FollowUpParams struct {
	Task               string          `json:"task"`
	ID                 string          `json:"id,omitempty"`
	Title              string          `json:"title,omitempty"`
	Prompt             string          `json:"prompt"`
	Scope              []string        `json:"scope,omitempty"`
	TurnBudget         *TurnBudgetView `json:"turn_budget,omitempty"`
	AuthorizingMessage string          `json:"authorizing_message,omitempty"`
}

// FollowUpResult is FollowUp RPC result.
type FollowUpResult struct {
	ID              string `json:"id"`
	PredecessorTask string `json:"predecessor_task"`
	State           string `json:"state"`
	ResumedSession  string `json:"resumed_session,omitempty"`
}

// PoolResizeParams is PoolResize RPC input (FAE-SCH-015).
type PoolResizeParams struct {
	Pool               string `json:"pool"`
	Cap                int    `json:"cap"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}

// QuotaSetParams is QuotaSet RPC input (FAE-RT-018).
type QuotaSetParams struct {
	Provider           string `json:"provider,omitempty"`
	Model              string `json:"model,omitempty"`
	Pool               string `json:"pool,omitempty"`
	ResetAt            string `json:"reset_at"`
	GraceSeconds       int    `json:"grace_seconds,omitempty"`
	Reason             string `json:"reason,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}

// QuotaClearParams is QuotaClear RPC input (FAE-RT-018).
type QuotaClearParams struct {
	Provider           string `json:"provider,omitempty"`
	Model              string `json:"model,omitempty"`
	Pool               string `json:"pool,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}

// DispatchPauseParams is DispatchPause / DispatchUnpause RPC input (FAE-RT-018).
type DispatchPauseParams struct {
	Scope              string `json:"scope"`
	Target             string `json:"target,omitempty"`
	Reason             string `json:"reason,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}

// DecideParams is Decide RPC input.
type DecideParams struct {
	Task               string `json:"task"`
	Stage              string `json:"stage,omitempty"`
	Approved           bool   `json:"approved"`
	Note               string `json:"note,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}

// ResolveUncertainParams is ResolveUncertain RPC input.
type ResolveUncertainParams struct {
	Task               string `json:"task"`
	Action             string `json:"action"`
	Note               string `json:"note,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}

// LifecycleParams covers ResumeWithContext, RestartWithInformation, QuarantineRelease, Retire, Land, etc.
type LifecycleParams struct {
	Task               string `json:"task"`
	Verb               string `json:"verb"`
	Context            string `json:"context,omitempty"`
	Reason             string `json:"reason,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}

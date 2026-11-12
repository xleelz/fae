// Package engine implements repository-neutral durable workflow decisions. The
// caller must persist Prepare's returned job before executing its dispatch and
// atomically persist Complete's returned job and events before dispatching again.
package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/workflow"
)

type Event struct {
	TemplatePresence map[string]bool        `json:"template_presence,omitempty"`
	Visit            int                    `json:"visit,omitempty"`
	Failure          *FailureRoute          `json:"failure,omitempty"`
	EvidenceIDs      []string               `json:"evidence_ids,omitempty"`
	BranchIndex      *int                   `json:"branch_index,omitempty"`
	BranchID         string                 `json:"branch_id,omitempty"`
	Request          *AncestorChangeRequest `json:"request,omitempty"`
	TurnStats        *domain.TurnStat       `json:"turn_stats,omitempty"`
	Kind             string                 `json:"kind"`
	Stage            string                 `json:"stage"`
	From             domain.State           `json:"from,omitempty"`
	To               domain.State           `json:"to,omitempty"`
	Attempt          int                    `json:"attempt"`
	Cause            string                 `json:"cause"`
	DispatchID       string                 `json:"dispatch_id,omitempty"`
	Gate             *workflow.GateResult   `json:"gate,omitempty"`
	// EscalationKind types every transition into Escalated (Appendix B).
	EscalationKind domain.EscalationKind `json:"escalation_kind,omitempty"`
	// Record is the content address of the evaluation's gate record (FAE-WF-063).
	Record string `json:"record,omitempty"`
}
type Dispatch struct {
	BranchInstance   string              `json:"branch_instance,omitempty"`
	BranchSiblings   []any               `json:"branch_siblings,omitempty"`
	TemplatePresence map[string]bool     `json:"template_presence,omitempty"`
	BranchIndex      *int                `json:"branch_index,omitempty"`
	OutputSchema     json.RawMessage     `json:"output_schema,omitempty"`
	TemplateValues   map[string]any      `json:"template_values,omitempty"`
	PriorRejection   []string            `json:"prior_rejection,omitempty"`
	ExecutionHead    string              `json:"execution_head,omitempty"`
	OperatorContext  string              `json:"operator_context,omitempty"`
	AttemptCharged   bool                `json:"attempt_charged,omitempty"`
	Base             string              `json:"base,omitempty"`
	ExecutionKind    string              `json:"execution_kind,omitempty"`
	ID               string              `json:"id"`
	TaskID           string              `json:"task_id"`
	Stage            string              `json:"stage"`
	Tree             string              `json:"tree"`
	Attempt          int                 `json:"attempt"`
	Visit            int                 `json:"visit"`
	Config           workflow.Stage      `json:"config"`
	Inputs           []workflow.Evidence `json:"inputs"`
	Sessions         []Session           `json:"sessions"`
	CreatedAt        time.Time           `json:"created_at"`

	// Argv is the HostCommand vector bound from Inputs (FAE-WF-035).
	Argv []string `json:"argv,omitempty"`
	// TaskClass selects the role overlays of the turn (FAE-WF-044).
	TaskClass string `json:"task_class,omitempty"`
	// Scope is the task's approved widened file set (FAE-WF-078).
	Scope []string `json:"scope,omitempty"`
	// RouteGeneration is the route generation (Options) the turn's bindings
	// were selected in; a binding refusal is recorded in it.
	RouteGeneration string `json:"route_generation,omitempty"`
	// FanOut branch metadata when dispatched inside a FanOut branch (FAE-WF-096).
	BranchID   string   `json:"branch_id,omitempty"`
	FanStage   string   `json:"fan_stage,omitempty"`
	BranchItem any      `json:"branch_item,omitempty"`
	WriteScope []string `json:"write_scope,omitempty"`
	// ResumeCheckpoint carries the durable checkpoint for an interrupted or
	// resume-once agentic turn (FAE-RT-015, FAE-RT-019).
	ResumeCheckpoint *TurnCheckpoint    `json:"resume_checkpoint,omitempty"`
	Notifications    []TaskNotification `json:"notifications,omitempty"`
	FollowUpOf       string             `json:"follow_up_of,omitempty"`
	TurnBudget       *TurnBudgetSpec    `json:"turn_budget,omitempty"`
}

// TaskNotification is a structured notification queued for or delivered to a
// task's brief as quoted data (FAE-OPS-033, FAE-WF-043).
type TaskNotification struct {
	ID          string    `json:"id"`
	TaskID      string    `json:"task_id"`
	FromTaskID  string    `json:"from_task_id,omitempty"`
	Kind        string    `json:"kind"`     // "breaking-change" | "cross-area-need" | "note"
	Delivery    string    `json:"delivery"` // "next-turn" | "in-flight"
	Body        string    `json:"body"`
	RelayedBy   string    `json:"relayed_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	DeliveredAt time.Time `json:"delivered_at,omitempty"`
}

// TurnBudgetSpec bounds a follow-up task's turn execution (FAE-OPS-034).
type TurnBudgetSpec struct {
	WallSeconds int `json:"wall_seconds,omitempty"`
	MaxTurns    int `json:"max_turns,omitempty"`
}
type Session struct {
	Route              workflow.Binding `json:"route,omitempty"`
	ID                 string           `json:"id"`
	Role               string           `json:"role"`
	TaskID             string           `json:"task_id,omitempty"`
	Priority           int              `json:"priority,omitempty"`
	Admitted           time.Time        `json:"admitted,omitempty"`
	CutOffByQuota      bool             `json:"cut_off_by_quota,omitempty"`
	CredentialIdentity string           `json:"credential_identity,omitempty"`
	ResumeSessionID    string           `json:"resume_session_id,omitempty"`
}
type Result struct {
	RuntimeMounts      map[string]string `json:"runtime_mounts,omitempty"`
	ViewConstructionMS int64             `json:"view_construction_ms,omitempty"`
	ViewCopiedBytes    int64             `json:"view_copied_bytes,omitempty"`
	IsolationPolicy    string            `json:"isolation_policy,omitempty"`
	Generated          bool              `json:"generated,omitempty"`
	TurnStats          []domain.TurnStat `json:"turn_stats,omitempty"`
	Detail             string            `json:"detail,omitempty"`
	NewBase            string            `json:"new_base,omitempty"`
	ExecutionKind      string            `json:"execution_kind,omitempty"`
	// NewTree is measured by the trusted live executor after an allowed change turn.
	NewTree            string          `json:"new_tree,omitempty"`
	BranchCommit       string          `json:"branch_commit,omitempty"`
	BranchRef          string          `json:"branch_ref,omitempty"`
	BranchBaseTree     string          `json:"branch_base_tree,omitempty"`
	DispatchID         string          `json:"dispatch_id"`
	TaskID             string          `json:"task_id"`
	Tree               string          `json:"tree"`
	Session            string          `json:"session"`
	InterruptedSession string          `json:"interrupted_session,omitempty"`
	QuotaResetAt       time.Time       `json:"quota_reset_at,omitempty"`
	Output             json.RawMessage `json:"output"`
	Reviews            []ReviewResult  `json:"reviews,omitempty"`
	Class              string          `json:"class,omitempty"`
	ProducedAt         time.Time       `json:"produced_at"`
	// Transcript points into the agent CLI's own store (FAE-OBS-007).
	Transcript *Transcript `json:"transcript,omitempty"`
	// RefusedRoute is the session binding a ClassBindingRefused result names.
	RefusedRoute *workflow.Binding `json:"refused_route,omitempty"`
	// Checkpoint is the latest durable agentic turn checkpoint (FAE-RT-019).
	Checkpoint            *TurnCheckpoint `json:"checkpoint,omitempty"`
	ResumedFromCheckpoint bool            `json:"resumed_from_checkpoint,omitempty"`
	// ReadRoots and Reads record the verified read roots (FAE-RT-016) and
	// effective read policy (FAE-SEC-008) of the turn or command.
	ReadRoots map[string]string    `json:"read_roots,omitempty"`
	Reads     *workflow.ReadPolicy `json:"reads,omitempty"`
}
type ReviewResult struct {
	Session    string               `json:"session"`
	Role       string               `json:"role"`
	Output     json.RawMessage      `json:"output"`
	Transcript *Transcript          `json:"transcript,omitempty"`
	ReadRoots  map[string]string    `json:"read_roots,omitempty"`
	Reads      *workflow.ReadPolicy `json:"reads,omitempty"`
}

// Transcript is a best-effort correlation reference to an agent turn in the
// provider CLI's own store: never raw text, only where to find it.
type Transcript struct {
	ProviderSession string    `json:"provider_session,omitempty"`
	LogDirectory    string    `json:"log_directory,omitempty"`
	Pane            string    `json:"pane,omitempty"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	FinishedAt      time.Time `json:"finished_at,omitempty"`
}
type Executor interface {
	Execute(context.Context, Dispatch) (Result, error)
}
type Provenance struct {
	RuntimeMounts      map[string]string  `json:"runtime_mounts,omitempty"`
	ViewConstructionMS int64              `json:"view_construction_ms,omitempty"`
	ViewCopiedBytes    int64              `json:"view_copied_bytes,omitempty"`
	IsolationPolicy    string             `json:"isolation_policy,omitempty"`
	Generated          bool               `json:"generated,omitempty"`
	ExecutionKind      string             `json:"execution_kind"`
	ArtifactID         string             `json:"artifact_id"`
	DispatchID         string             `json:"dispatch_id"`
	TaskID             string             `json:"task_id"`
	Stage              string             `json:"stage"`
	Primitive          workflow.Primitive `json:"primitive"`
	Role               string             `json:"role"`
	Session            string             `json:"session"`
	Tree               string             `json:"tree"`
	Inputs             []string           `json:"inputs"`
	ProducedAt         time.Time          `json:"produced_at"`
	WorkflowHash       string             `json:"workflow_hash"`
	WorkflowRevision   int                `json:"workflow_revision"`
	Provider           string             `json:"provider,omitempty"`
	Model              string             `json:"model,omitempty"`
	Transcript         *Transcript        `json:"transcript,omitempty"`
	// CarriedFrom names the earlier task whose accepted pre-code artifact
	// this re-admission carried forward (FAE-WF-086).
	CarriedFrom string `json:"carried_from,omitempty"`
	// BranchID identifies the FanOut branch that produced this artifact (FAE-WF-096).
	BranchID string `json:"branch_id,omitempty"`
	// Checkpoint provenance fields (FAE-RT-019).
	CheckpointSession     string               `json:"checkpoint_session,omitempty"`
	CheckpointTree        string               `json:"checkpoint_tree,omitempty"`
	ResumedFromCheckpoint bool                 `json:"resumed_from_checkpoint,omitempty"`
	ReadRoots             map[string]string    `json:"read_roots,omitempty"`
	Reads                 *workflow.ReadPolicy `json:"reads,omitempty"`
}

// DocumentCheckReceipt records the result of a target DocumentCheck (FAE-CTL-052).
type DocumentCheckReceipt struct {
	ID         string `json:"id"`
	ExitCode   int    `json:"exit_code"`
	CountField string `json:"count_field"`
	Count      int    `json:"count"`
	ParseError string `json:"parse_error,omitempty"`
}
type Job struct {
	BranchAttemptPaid        bool                      `json:"branch_attempt_paid,omitempty"`
	SlotBinding              *WorkflowSlotBinding      `json:"slot_binding,omitempty"`
	SlotWaitSince            time.Time                 `json:"slot_wait_since,omitempty"`
	SlotWaitReason           string                    `json:"slot_wait_reason,omitempty"`
	SlotHolder               string                    `json:"slot_holder,omitempty"`
	Footprint                *workflow.Footprint       `json:"footprint,omitempty"`
	BranchVisits             map[string]map[string]int `json:"branch_visits,omitempty"`
	BranchContext            *BranchContext            `json:"branch_context,omitempty"`
	DecisionItems            []any                     `json:"decision_items,omitempty"`
	IncomingRequests         []AncestorChangeRequest   `json:"incoming_requests,omitempty"`
	AncestorRequests         []AncestorChangeRequest   `json:"ancestor_requests,omitempty"`
	Ancestors                []string                  `json:"ancestors,omitempty"`
	RunStats                 map[string]any            `json:"run_stats,omitempty"`
	resumeStageAuthorization string
	fanCursorAuthorization   string
	TurnStats                []domain.TurnStat   `json:"turn_stats,omitempty"`
	EscalatedStage           string              `json:"escalated_stage,omitempty"`
	PriorRejections          map[string][]string `json:"prior_rejections,omitempty"`
	HandlerAliases           map[string]string   `json:"handler_aliases,omitempty"`
	Inputs                   map[string]any      `json:"inputs,omitempty"`
	InputSources             map[string][]string `json:"input_sources,omitempty"`
	TreeRoot                 string              `json:"tree_root,omitempty"`
	TreeDepth                int                 `json:"tree_depth,omitempty"`
	TreeDescendants          int                 `json:"tree_descendants,omitempty"`
	RootMaxDescendants       int                 `json:"root_max_descendants,omitempty"`
	Detached                 bool                `json:"detached,omitempty"`
	MaxSessions              int                 `json:"max_sessions,omitempty"`
	OutputDir                string              `json:"output_dir,omitempty"`
	StagingDir               string              `json:"staging_dir,omitempty"`

	// ExecutionHead is broker-proved branch HEAD; Base remains the reconciliation origin.
	ExecutionHead     string         `json:"execution_head,omitempty"`
	AttemptCapStage   string         `json:"attempt_cap_stage,omitempty"`
	NewAttempt        bool           `json:"new_attempt,omitempty"`
	BlockedFrom       string         `json:"blocked_from,omitempty"`
	TaskAttemptLimit  int            `json:"task_attempt_limit,omitempty"`
	ContextEntries    []ContextEntry `json:"context_entries,omitempty"`
	Deferred          bool           `json:"deferred,omitempty"`
	DeferredUntil     time.Time      `json:"deferred_until,omitempty"`
	BlockedRoute      string         `json:"blocked_route,omitempty"`
	OperatorStage     string         `json:"operator_stage,omitempty"`
	OperatorReason    string         `json:"operator_reason,omitempty"`
	ReviewPollCount   int            `json:"review_poll_count,omitempty"`
	ReviewPollSeconds int            `json:"review_poll_seconds,omitempty"`
	ReviewMaxPolls    int            `json:"review_max_polls,omitempty"`

	ReviewRevisionStage string            `json:"review_revision_stage,omitempty"`
	ReviewStages        map[string]string `json:"review_stages,omitempty"`
	ReviewMergeRequired bool              `json:"review_merge_required,omitempty"`
	ReviewMerged        bool              `json:"review_merged,omitempty"`
	ReviewDisabled      bool              `json:"review_disabled,omitempty"`
	ReviewID            string            `json:"review_id,omitempty"`
	ReviewRevision      int               `json:"review_revision,omitempty"`
	ReviewMaxRevisions  int               `json:"review_max_revisions,omitempty"`
	ReviewRemediation   string            `json:"review_remediation,omitempty"`

	Disposition         domain.Disposition `json:"disposition,omitempty"`
	AdmissionRetryAfter time.Time          `json:"admission_retry_after,omitempty"`
	Base                string             `json:"base,omitempty"`
	// ExecutionKind pins the executor trust boundary for the job lifetime.
	ExecutionKind      string              `json:"execution_kind"`
	TaskID             string              `json:"task_id"`
	Revision           int                 `json:"revision"`
	Document           workflow.Document   `json:"document"`
	WorkflowHash       string              `json:"workflow_hash"`
	State              domain.State        `json:"state"`
	Tree               string              `json:"tree"`
	Attempts           int                 `json:"attempts"`
	TestRuns           int                 `json:"test_runs"`
	Counts             map[string]int      `json:"counts"`
	Failures           map[string]int      `json:"failures"`
	Sequence           int                 `json:"sequence"`
	ResolvedDispatches []Dispatch          `json:"resolved_dispatches,omitempty"`
	InFlight           *Dispatch           `json:"in_flight,omitempty"`
	Evidence           []workflow.Evidence `json:"evidence"`
	Provenance         []Provenance        `json:"provenance"`
	History            []Event             `json:"history"`
	Reason             string              `json:"reason,omitempty"`
	// EscalationKind is the closed Appendix B kind of the current or retired
	// escalation; EscalationDetail names the specific cause.
	EscalationKind   domain.EscalationKind `json:"escalation_kind,omitempty"`
	EscalationDetail string                `json:"escalation_detail,omitempty"`
	// RoutesUnavailableSince starts the route-exhaustion bound of a no-route park.
	RoutesUnavailableSince time.Time `json:"routes_unavailable_since,omitempty"`
	// QuotaResetAt records the provider usage-limit reset time T for a
	// ProviderQuota park (FAE-RT-018).
	QuotaResetAt time.Time `json:"quota_reset_at,omitempty"`
	// InterruptedSessionID and CutOffByQuota record an in-flight turn cut off
	// by a provider usage limit so it resumes first at T in its provider session
	// with its kept in-scope worktree changes (FAE-RT-018, FAE-RT-019).
	InterruptedSessionID string `json:"interrupted_session_id,omitempty"`
	CutOffByQuota        bool   `json:"cut_off_by_quota,omitempty"`
	// RefusedRoutes are the bindings the bridge refused for this task before
	// the provider ran (ClassBindingRefused); route selection skips them.
	RefusedRoutes []RouteRefusal `json:"refused_routes,omitempty"`
	// Parked is a derived wait reason; State remains the durable stage.
	Parked bool `json:"parked"`
	// GateRecords are the content-addressed gate, branch-arm, aggregate and
	// shape evaluations, replayable over the recorded evidence (FAE-WF-063).
	GateRecords []workflow.GateRecord `json:"gate_records,omitempty"`
	// Fan is the durable branch and join state of an active FanOut visit.
	Fan     *FanState `json:"fan,omitempty"`
	LastFan *FanState `json:"last_fan,omitempty"`
	// Child is the sub-workflow task a SubWorkflow visit waits for; Parent
	// links a child task back to that visit.
	Child  *ChildRef  `json:"child,omitempty"`
	Parent *ParentRef `json:"parent,omitempty"`
	// TaskClass is the task's declared class, pinned at admission (FAE-WF-044).
	TaskClass string `json:"task_class,omitempty"`
	// Governed requests (FAE-WF-078): the request awaiting its judge, the
	// durable per-kind counters and the approved scope carried forward.
	PendingRequest *Request       `json:"pending_request,omitempty"`
	RequestCounts  map[string]int `json:"request_counts,omitempty"`
	ApprovedScope  []string       `json:"approved_scope,omitempty"`
	// LoopProgress is each progress-aware Loop's last measure (FAE-WF-075).
	LoopProgress map[string]LoopProgress `json:"loop_progress,omitempty"`
	// Carries are patch-identical reconciles whose approvals stay valid (vcs.go).
	Carries []ReviewCarry `json:"carries,omitempty"`
	// CarryEntry is the stage a Pending carry-forward re-admission enters
	// when it is released (FAE-WF-086, carry.go).
	CarryEntry string `json:"carry_entry,omitempty"`
	// DeclaredFiles lists the task's declared editable files (FAE-WF-096, FAE-CTL-052).
	DeclaredFiles []string `json:"declared_files,omitempty"`
	// DocumentGlobs and DocumentChecks record document-only task policy and receipts (FAE-CTL-052).
	DocumentGlobs  []string               `json:"document_globs,omitempty"`
	DocumentChecks []DocumentCheckReceipt `json:"document_checks,omitempty"`
	// TurnCheckpoints holds durable agentic turn checkpoints by stage (FAE-RT-019).
	TurnCheckpoints map[string]TurnCheckpoint `json:"turn_checkpoints,omitempty"`
	ReadRoots       map[string]string         `json:"read_roots,omitempty"`
	Reads           *workflow.ReadPolicy      `json:"reads,omitempty"`
	DenyPaths       []string                  `json:"deny_paths,omitempty"`
	Notifications   []TaskNotification        `json:"notifications,omitempty"`
	FollowUpOf      string                    `json:"follow_up_of,omitempty"`
	TurnBudget      *TurnBudgetSpec           `json:"turn_budget,omitempty"`
	ReadOnlyFiles   []string                  `json:"read_only_files,omitempty"`
}

func New(taskID string, revision int, doc workflow.Document, tree string) (Job, error) {
	j := Job{ExecutionKind: "scripted", TaskID: taskID, Revision: revision, Document: doc, State: domain.Pending, Tree: tree, Counts: map[string]int{}, Failures: map[string]int{}}
	if taskID == "" || revision < 1 || tree == "" {
		return j, errors.New("task, revision and tree required")
	}
	if issues := workflow.Validate(doc); len(issues) > 0 {
		return j, fmt.Errorf("invalid workflow: %v", issues)
	}
	var err error
	j.WorkflowHash, err = workflow.Hash(doc)
	return clone(j), err
}

// NewLive creates a job for the trusted live executor. Scripted records cannot
// be relabeled: every artifact and dispatch must retain its execution kind.
func NewLive(taskID string, revision int, doc workflow.Document, tree string) (Job, error) {
	j, err := New(taskID, revision, doc, tree)
	if err == nil {
		j.ExecutionKind = "live"
	}
	return j, err
}
func clone(j Job) Job {
	b, _ := json.Marshal(j)
	var out Job
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	_ = dec.Decode(&out)
	return out
}

// Validate checks immutable workflow binding and stored artifact identities. It
// does not authenticate arbitrary JSON: snapshots must come from the trusted store.
func Validate(j Job) error {
	if b := j.SlotBinding; b != nil {
		if b.Holder == "" || b.Workflow == "" || (b.Scope != "molecule" && b.Scope != "tree") || b.Revision < 1 || b.Sessions < 0 || b.Sessions > 1000000 || b.Memory < 0 || b.Panes < 0 || b.Panes > b.Sessions {
			return errors.New("invalid granted workflow slot binding")
		}
	}
	if b := j.SlotBinding; b != nil {
		count := b.BaseSessions
		if count < 0 || count > b.Sessions || b.BaseMemory < 0 || b.BaseMemory > b.Memory {
			return errors.New("invalid granted workflow baseline")
		}
		for id, n := range b.DetachedSessions {
			if id == "" || n < 1 || n > 1000000-count {
				return errors.New("invalid granted detached footprint")
			}
			count += n
		}
		if count > b.Sessions {
			return errors.New("granted detached footprints exceed commitment")
		}
	}
	if j.TaskAttemptLimit < 0 {
		return errors.New("invalid task attempt limit")
	}
	if _, ok := j.Document.TaskClasses[j.TaskClass]; j.TaskClass != "" && !ok {
		return errors.New("undeclared task class")
	}
	if e := validRequests(j); e != nil {
		return e
	}
	if j.EscalationKind != "" && (!j.EscalationKind.Valid() || (j.State != domain.Escalated && j.State != domain.Retired)) {
		return errors.New("invalid escalation kind")
	}
	for _, entry := range j.ContextEntries {
		if entry.SHA256 != ContextHash(entry.Text) {
			return errors.New("invalid task context hash")
		}
	}
	for _, n := range j.Notifications {
		if !ValidNotificationKind(n.Kind) || !ValidNotificationDelivery(n.Delivery) || strings.TrimSpace(n.Body) == "" || n.TaskID == "" {
			return errors.New("invalid task notification")
		}
	}
	if j.OperatorStage != "" && (j.InFlight == nil || j.OperatorStage != j.InFlight.Stage || (j.State != domain.Blocked && j.State != domain.Escalated) || j.OperatorReason == "") {
		return errors.New("invalid operator hold")
	}
	if j.Disposition.Mode != "" && j.Disposition.Mode != domain.Local && j.Disposition.Mode != domain.Normal {
		return errors.New("invalid job disposition")
	}
	if j.Disposition.Live && (j.Disposition.Mode != domain.Normal || j.Disposition.Destination == "" || j.Disposition.PushNamespace == "") {
		return errors.New("invalid live external disposition")
	}
	if j.Disposition.Mode == domain.Normal && j.ExecutionKind != "live" {
		return errors.New("Normal disposition requires trusted runtime")
	}

	if j.ReviewPollCount < 0 || j.ReviewMaxPolls < 0 || j.ReviewMaxPolls > 3600 || j.ReviewPollSeconds < 0 || j.ReviewPollSeconds > 3600 {
		return errors.New("invalid review poll bounds")
	}
	if j.ExecutionKind != "scripted" && j.ExecutionKind != "live" {
		return errors.New("unsupported or missing execution kind")
	}
	h, e := workflow.Hash(j.Document)
	if e != nil || h != j.WorkflowHash {
		return errors.New("workflow binding hash mismatch")
	}
	if j.TaskID == "" || j.Revision < 1 || j.Tree == "" || j.Counts == nil || j.Failures == nil {
		return errors.New("incomplete job")
	}
	if j.CarryEntry != "" && (j.State != domain.Pending || !workflow.CarryEntry(j.Document, j.CarryEntry)) {
		return errors.New("carry entry outside a Pending task or not a carry-entry stage")
	}
	if j.Attempts < 0 || j.TestRuns < 0 || j.Sequence < 0 {
		return errors.New("negative counters")
	}
	for _, counts := range []map[string]int{j.Counts, j.Failures} {
		for _, n := range counts {
			if n < 0 {
				return errors.New("negative stage counter")
			}
		}
	}
	for _, a := range j.Evidence {
		if a.Type == "failure" && strings.HasPrefix(a.Stage, "failure:") {
			if err := validateFailureEvidence(j, a); err != nil {
				return err
			}
			continue
		}
		if handled, err := validateTreeEvidence(j, a); handled {
			if err != nil {
				return err
			}
			continue
		}
		if handled, err := validateRunStatsEvidence(j, a); handled {
			if err != nil {
				return err
			}
			continue
		}
		if handled, err := validateInputEvidence(j, a); handled {
			if err != nil {
				return err
			}
			continue
		}
		stage, exists := j.Document.Stages[a.Stage]
		if !exists || !workflow.StageProducesType(stage, a.Type) {
			return errors.New("artifact does not match producer stage output")
		}
		b, e := workflow.EvidenceJSON(a)
		if e != nil {
			return e
		}
		id, e := domain.ContentHash(b)
		if e != nil || a.ID != id || a.TaskID != j.TaskID || !a.Provenance {
			return errors.New("invalid stored evidence identity")
		}
		typ, ok := j.Document.Artifacts[a.Type]
		if !ok || typ.Version != a.Version {
			return errors.New("unknown stored artifact type")
		}
		if e = workflow.ValidateArtifact(typ.Schema, b); e != nil {
			return e
		}
		found := false
		for _, p := range j.Provenance {
			if p.ArtifactID == a.ID && p.TaskID == a.TaskID && p.Stage == a.Stage && p.Tree == a.Tree && p.Session == a.Session && p.Role == a.Role && p.DispatchID != "" && p.Primitive == stage.Primitive && !p.ProducedAt.IsZero() && p.ExecutionKind == j.ExecutionKind {
				found = true
			}
		}
		if !found {
			return errors.New("missing stored evidence provenance")
		}
	}
	if err := validateRecords(j); err != nil {
		return err
	}
	if err := validateFanState(j); err != nil {
		return err
	}
	if err := validateChild(j); err != nil {
		return err
	}
	dispatchOwner := j
	nestedAlias := j.Fan != nil && j.Fan.Baseline != nil
	if nestedAlias {
		for _, b := range j.Fan.Branches {
			if b.Frame != nil {
				if err := Validate(*b.Frame); err != nil {
					return fmt.Errorf("branch %s: %w", b.ID, err)
				}
			}
		}
		if j.InFlight != nil {
			owner, ok := BranchJob(j, j.InFlight.BranchID)
			if !ok || owner.InFlight == nil || owner.Fan != nil {
				return errors.New("nested dispatch has no authoritative leaf frame")
			}
			alias, _ := json.Marshal(j.InFlight)
			leaf, _ := json.Marshal(owner.InFlight)
			if !bytes.Equal(alias, leaf) {
				return errors.New("nested dispatch alias differs from leaf frame")
			}
			dispatchOwner = owner
		}
	}
	if j.ExecutionHead != "" && j.ExecutionHead != j.Base {
		known := j.LatestCommitHead() == j.ExecutionHead
		if !known {
			return errors.New("execution HEAD lacks latest broker commit provenance")
		}
	}
	if j.InFlight != nil && j.InFlight.ExecutionHead != "" && j.InFlight.ExecutionHead != dispatchOwner.ExpectedHead() {
		return errors.New("in-flight execution HEAD mismatch")
	}
	if j.InFlight != nil && (j.InFlight.TaskID != j.TaskID || (j.InFlight.Stage != string(dispatchOwner.State) && j.InFlight.Stage != j.OperatorStage) || j.InFlight.Tree != dispatchOwner.Tree || j.InFlight.Base != dispatchOwner.Base) {
		return errors.New("invalid in-flight dispatch")
	}
	if d := j.InFlight; d != nil {
		if d.ExecutionKind != j.ExecutionKind && !(j.ExecutionKind == "scripted" && d.ExecutionKind == "") {
			return errors.New("dispatch execution kind mismatch")
		}
		stored, _ := json.Marshal(d.Config)
		declared, _ := json.Marshal(j.Document.Stages[d.Stage])
		if !bytes.Equal(stored, declared) || len(d.Sessions) == 0 || d.ID == "" || d.CreatedAt.IsZero() {
			return errors.New("invalid dispatch configuration")
		}
		if e := checkArgv(*d); e != nil {
			return e
		}
		validID := d.ID == fmt.Sprintf("%s/%s/%d", j.TaskID, j.WorkflowHash, j.Sequence)
		validVisit := d.Visit == dispatchOwner.Counts[d.Stage] && d.Visit >= 1
		validAttempt := d.Attempt == dispatchOwner.Attempts && d.Attempt >= 1
		if j.Fan != nil && (j.Fan.Parallel || nestedAlias) {
			for seq := 1; seq <= j.Sequence; seq++ {
				if d.ID == fmt.Sprintf("%s/%s/%d", j.TaskID, j.WorkflowHash, seq) {
					validID = true
					break
				}
			}
			validVisit = d.Visit >= 1 && d.Visit <= dispatchOwner.Counts[d.Stage]
			validAttempt = d.Attempt >= 1 && d.Attempt <= dispatchOwner.Attempts
		}
		if !validID || !validAttempt || !validVisit {
			return errors.New("invalid dispatch identity or counters")
		}
		roles := []string{d.Config.Role}
		count := 1
		if d.Config.Primitive == workflow.Review {
			if len(d.Config.Reviewers) > 0 {
				roles = d.Config.Reviewers
			}
			if d.Config.Count > 0 {
				count = d.Config.Count
			}
		}
		if len(d.Sessions) != len(roles)*count {
			return errors.New("invalid dispatch session count")
		}
		for i, session := range d.Sessions {
			if session.Route.Provider != "" {
				declared := false
				for _, route := range j.roleRoutes(session.Role) {
					if route == session.Route {
						declared = true
					}
				}
				if !declared {
					return errors.New("dispatch route not declared for role")
				}
			}
			if session.ID != fmt.Sprintf("%s/session/%d", d.ID, i) || session.Role != roles[i/count] {
				return errors.New("invalid dispatch session identity")
			}
		}
	}
	return nil
}
func event(j *Job, e Event, events *[]Event) {
	j.History = append(j.History, e)
	*events = append(*events, e)
}
func move(j *Job, to, cause string, events *[]Event) error {
	if to == string(domain.Done) {
		if missing := missingDeclaredOutputs(*j); len(missing) > 0 {
			stop(j, domain.HumanDecisionRequired, "MissingWorkflowOutput: "+strings.Join(missing, ", "), events)
			return nil
		}
	}

	fanArrive(j, to, events)
	if to == string(domain.Done) && j.ReviewMergeRequired && !j.ReviewMerged {
		stop(j, domain.ReviewGateFailed, "ReviewMergeRequired: Done requires the review merge receipt", events)
		return nil
	}
	if to == string(domain.Done) && j.Disposition.Mode == domain.Normal && !j.Disposition.Live {
		return transition(j, domain.Escalation{Kind: domain.DryRunComplete}, string(domain.Escalated), string(domain.DryRunComplete), events)
	}
	return transition(j, domain.Escalation{}, to, cause, events)
}

// enter is the only change of a task's durable state (FAE-WF-008): kernel
// verbs, carry-forward, migration and stage routing all move along an edge
// that workflow.CanTransition accepts for the bound content. A carry entry
// belongs to a Pending task only (Validate): leaving Pending by any edge drops
// it, so an operator Escalate or Cancel of a carried task stores a valid row.
// The release reads the entry before it enters the carried stage.
//
// Agent-start, failover and stall retries re-dispatch the turn at its stage,
// so leaving the stage by any edge ends that turn: an escalation
// (StallExhausted, AttemptCap or any other kind), a block, a restart, a
// resume or a migration remap. Their bounds are per turn (Appendix A;
// FAE-RT-004, -005, -010), so their counters are cleared here and the next
// turn starts fresh ones. They are not per-task budgets, so this refunds
// nothing (FAE-WF-074).
func enter(j *Job, to domain.State) error {
	if !workflow.CanTransition(j.Document, j.State, to, j.resumeStageAuthorization) && j.fanCursorAuthorization != string(to) {
		return fmt.Errorf("illegal transition %s -> %s", j.State, to)
	}
	if j.State == domain.Pending && to != domain.Pending {
		j.CarryEntry = ""
	}
	if to != j.State {
		clearTurnBounds(j)
		j.Child = nil
	}
	j.State = to
	j.resumeStageAuthorization = ""
	j.fanCursorAuthorization = ""
	return nil
}
func transition(j *Job, escalation domain.Escalation, to, cause string, events *[]Event) error {
	from := j.State
	if to == string(domain.Escalated) && from != domain.Escalated {
		j.EscalatedStage = string(from)
	}
	if err := enter(j, domain.State(to)); err != nil {
		return err
	}
	event(j, Event{Kind: "transition", Stage: string(from), From: from, To: domain.State(to), Attempt: j.stagedAttempt(), Cause: cause, EscalationKind: escalation.Kind}, events)
	j.Reason = ""
	if escalation.Kind != "" {
		j.Reason = escalation.String()
	}
	j.EscalationKind = escalation.Kind
	j.EscalationDetail = escalation.Detail
	j.Parked = false
	return nil
}

// stop escalates with exactly one Appendix B kind; detail names the cause.
func stop(j *Job, kind domain.EscalationKind, detail string, events *[]Event) {
	if j.State.Terminal() {
		return
	}
	if j.State != domain.Escalated {
		j.EscalatedStage = string(j.State)
	}
	if kind == domain.AttemptCap {
		j.AttemptCapStage = string(j.State)
	}
	escalation := domain.Escalation{Kind: kind, Detail: detail}
	if j.State == domain.Escalated {
		// An operator hold is already Escalated: retype it without a second transition.
		event(j, Event{Kind: "escalation", Stage: string(j.State), Attempt: j.stagedAttempt(), Cause: escalation.String(), EscalationKind: kind}, events)
	} else {
		_ = transition(j, escalation, string(domain.Escalated), escalation.String(), events)
	}
	j.Reason = escalation.String()
	j.EscalationKind = kind
	j.EscalationDetail = detail
	j.InFlight = nil
}

// stagedAttempt is the attempt every record stages explicitly (FAE-ROB-018):
// the charged attempt count, or attempt 1 while the first attempt has not yet
// charged a dispatch (admission, pre-dispatch gates and parks).
func (j Job) stagedAttempt() int {
	if j.Attempts == 0 {
		return 1
	}
	return j.Attempts
}

// Escalated reports the typed kind, accepting legacy untyped reasons.
func (j Job) Escalated(kind domain.EscalationKind) bool {
	return j.EscalationKind == kind || (j.EscalationKind == "" && j.Reason == string(kind))
}
func route(j Job, s workflow.Stage) (string, error) {
	next := ""
	for _, e := range j.Document.Edges {
		// An ExternalEffect's failure route is never its success route (as the
		// validator proves), whether or not a review lifecycle drives it.
		failure := e.To == s.Failure && (j.ReviewStages[string(j.State)] != "" || workflow.FailureRouted(s) || s.Primitive == workflow.ExternalEffect) && !workflow.SharedJoinSuccess(j.Document, string(j.State), e.To)
		if e.From == string(j.State) && !failure && e.To != s.Reverify && !workflow.RequestEdge(j.Document, e.From, e.To) && (!workflow.TurnBudgetExhaustionEdge(j.Document, e.From, e.To) || workflow.SharedJoinSuccess(j.Document, e.From, e.To)) {
			if next != "" {
				return "", errors.New("ambiguous success route")
			}
			next = e.To
		}
	}
	if next == "" {
		return "", errors.New("missing success route")
	}
	return next, nil
}
func current(j Job) []workflow.Evidence {
	activeBranch := ""
	if j.Fan != nil {
		if _, b := j.Fan.activeBranch(); b != nil {
			activeBranch = b.ID
		}
	}
	latest := map[string]string{}
	for _, p := range j.Provenance {
		if p.Tree == j.Tree {
			if activeBranch != "" && p.BranchID != "" && p.BranchID != activeBranch {
				continue
			}
			latest[p.Stage] = p.DispatchID
		}
	}
	out := []workflow.Evidence{}
	for _, a := range j.Evidence {
		if a.Tree != j.Tree {
			continue
		}
		for _, p := range j.Provenance {
			if p.ArtifactID == a.ID && p.Stage == a.Stage && p.Session == a.Session && p.DispatchID == latest[a.Stage] {
				out = append(out, a)
				break
			}
		}
	}
	return withRunStats(j, handlerEvidence(j, withTaskData(j, fanVisible(j, out))))
}

// Informational inputs and frozen identities survive tree changes. Gates and
// protected effects continue to use current-tree evidence exclusively.
func latestInputs(j Job) []workflow.Evidence {
	if j.ExecutionKind != "live" {
		return current(j)
	}
	activeBranch := ""
	if j.Fan != nil {
		if _, b := j.Fan.activeBranch(); b != nil {
			activeBranch = b.ID
		}
	}
	latest := map[string]string{}
	for _, p := range j.Provenance {
		if activeBranch != "" && p.BranchID != "" && p.BranchID != activeBranch {
			continue
		}
		latest[p.Stage] = p.DispatchID
	}
	var out []workflow.Evidence
	for _, a := range j.Evidence {
		for _, p := range j.Provenance {
			if p.ArtifactID == a.ID && p.Stage == a.Stage && p.Session == a.Session && p.DispatchID == latest[a.Stage] {
				out = append(out, a)
				break
			}
		}
	}
	return withRunStats(j, handlerEvidence(j, withTaskData(j, fanVisible(j, out))))
}
func boundInputs(j Job, s workflow.Stage) []workflow.Evidence {
	out := []workflow.Evidence{}
	seen := map[string]bool{}
	for _, a := range latestInputs(j) {
		for _, in := range s.Inputs {
			if a.Type == in.Type && a.Version == in.Version && (in.Stage == "" || a.Stage == in.Stage) {
				key := a.ID + "/" + a.Session
				if !seen[key] {
					out = append(out, a)
					seen[key] = true
				}
				break
			}
		}
	}
	return withRunStats(j, out)
}

// Prepare makes one durable decision. An existing in-flight dispatch is never
// silently re-issued: after an unplanned restart the caller must park or recover
// that token explicitly. A completed stage is never executed again.
func Prepare(input Job) (Job, *Dispatch, []Event, error) { return PrepareWithRoutes(input, nil) }

// PrepareWithRoutes pins selected bindings before launch. A nil selector retains
// the first declared binding for scripted and legacy callers.
func PrepareWithRoutes(input Job, selectRoute func([]workflow.Binding) (workflow.Binding, bool)) (Job, *Dispatch, []Event, error) {
	return PrepareWith(input, Options{SelectRoute: selectRoute})
}

// DefaultRouteExhaustion bounds a no-eligible-route park before ProviderUnavailable.
const DefaultRouteExhaustion = 30 * time.Minute

// SameGateRepeats is the Appendix A same-gate repeat bound.
const SameGateRepeats = 3

// ParkProviderQuota names FAE-RT-018's zero-charge park when all candidate
// provider pools or bindings are marked quota-exhausted until T, or when an
// in-flight turn is cut off by a provider usage limit with reset time T.
const ParkProviderQuota = "ProviderQuota"

type Options struct {
	ValidateChangeRequest func(Job, workflow.Stage) error
	SelectRoute           func([]workflow.Binding) (workflow.Binding, bool)
	// QuotaExhausted reports whether candidate routes are unavailable because
	// their provider pool or binding is marked quota-exhausted until resetAt T
	// (FAE-RT-018).
	QuotaExhausted func([]workflow.Binding) (time.Time, bool)
	// RouteExhaustion is the operator's route-exhaustion bound; zero selects the default.
	RouteExhaustion time.Duration
	// RouteGeneration names the configuration the bridge runs bindings
	// under; the daemon starts a new one each time it starts. A binding the
	// bridge refused is skipped only within the generation it was refused in.
	RouteGeneration string
	Priority        int
	Admitted        time.Time
	CredentialFor   func(workflow.Binding) string
}

func PrepareWith(input Job, o Options) (Job, *Dispatch, []Event, error) {
	if input.State.Terminal() || input.State == domain.Escalated {
		return input, nil, nil, Validate(input)
	}
	selectRoute := o.SelectRoute
	if o.RouteExhaustion <= 0 {
		o.RouteExhaustion = DefaultRouteExhaustion
	}
	j := clone(input)
	events := []Event{}
	if e := Validate(j); e != nil {
		return input, nil, nil, e
	}
	if j.Fan != nil && !inFan(j) {
		event(&j, Event{Kind: "fan-abandoned", Stage: j.Fan.Stage, Attempt: j.stagedAttempt(), Cause: fmt.Sprintf("visit %d left at %s", j.Fan.Visit, j.State)}, &events)
		j.Fan = nil
	}
	if j.Fan != nil && j.Fan.Baseline != nil && !j.State.Terminal() && j.State != domain.Escalated && !j.Deferred && j.OperatorStage == "" {
		next, dispatches, ev, err := prepareNestedFan(j, o, 1, "")
		if len(dispatches) > 0 {
			return next, dispatches[0], ev, err
		}
		return next, nil, ev, err
	}
	if j.Deferred {
		if j.DeferredUntil.IsZero() || time.Now().Before(j.DeferredUntil) {
			return j, nil, events, nil
		}
		j.Deferred = false
		j.DeferredUntil = time.Time{}
		event(&j, Event{Kind: "undefer", Attempt: j.stagedAttempt(), Cause: "defer deadline elapsed"}, &events)
	}
	if j.Parked && j.Reason == "infrastructure-deferred" && time.Now().Before(j.AdmissionRetryAfter) {
		return j, nil, events, nil
	}
	// A route park is decided again each time. A park on the bridge's
	// refusals is decided again without a selector too: a new route
	// generation (a daemon restart) resumes it in place (FAE-RT-010).
	if j.InFlight == nil && j.Parked && (j.Reason == ParkProviderQuota || selectRoute != nil && (j.Reason == ParkRoutesUnavailable || j.Reason == "infrastructure-deferred" || j.Reason == ParkProviderUnavailable) || routesRefusedPark(j.Reason)) {
		j.Parked = false
		j.Reason = ""
	}
	if j.Parked && j.Reason == "ReviewPending" && !time.Now().Before(j.AdmissionRetryAfter) {
		j.Parked = false
		j.Reason = ""
	}
	backedOff := j.Parked && j.Reason == ParkLoopBackoff && !time.Now().Before(j.AdmissionRetryAfter)
	if backedOff {
		j.Parked, j.Reason = false, ""
	}
	if j.State.Terminal() || j.State == domain.Escalated || j.InFlight != nil || j.Parked {
		return j, nil, events, nil
	}
	if j.State == domain.Pending {
		to, cause := j.Document.Start, "admitted"
		if j.CarryEntry != "" {
			to, cause = j.CarryEntry, "carry-forward: released at the carried evidence's entry"
		}
		// Leaving Pending drops the entry (enter).
		if e := move(&j, to, cause, &events); e != nil {
			return input, nil, nil, e
		}
		return j, nil, events, nil
	}
	// A stored approved path a widen may no longer add would refuse every
	// later dispatch: it is dropped, with an event, before the next one.
	dropRefusedApprovedScope(&j, &events)
	s, ok := j.Document.Stages[string(j.State)]
	if !ok {
		return input, nil, nil, errors.New("unknown durable stage")
	}
	if j.Fan != nil && !inFan(j) {
		// Left the fan without its join (restart or escalation route).
		event(&j, Event{Kind: "fan-abandoned", Stage: j.Fan.Stage, Attempt: j.stagedAttempt(), Cause: fmt.Sprintf("visit %d left at %s", j.Fan.Visit, j.State)}, &events)
		j.Fan = nil
	}
	if j.ReviewStages[string(j.State)] == "review_create" && j.ReviewID != "" {
		if j.ReviewRevisionStage == "" || j.ReviewRevision >= j.ReviewMaxRevisions {
			stop(&j, domain.ReviewGateFailed, "ReviewRevisionCap: same-review revision route unavailable or exhausted", &events)
			return j, nil, events, nil
		}
		if e := move(&j, j.ReviewRevisionStage, "existing review requires revision", &events); e != nil {
			return input, nil, nil, e
		}
		return j, nil, events, nil
	}
	if err := recordRunStats(&j, s); err != nil {
		return input, nil, nil, err
	}
	if missing := workflow.CheckInputs(s, j.TaskID, latestInputs(j)); len(missing) > 0 {
		if s.Failure != "" {
			if err := routeFailure(&j, "binding", fmt.Sprintf("MissingInput: %v", missing), "", &events); err != nil {
				return input, nil, nil, err
			}
			return j, nil, events, nil
		}
		j.Parked = true
		j.Reason = fmt.Sprintf("MissingInput: %v", missing)
		event(&j, Event{Kind: "park", Stage: string(j.State), Attempt: j.stagedAttempt(), Cause: j.Reason}, &events)
		return j, nil, events, nil
	}
	if s.Primitive == workflow.Escalate {
		kind, detail := s.Kind, "workflow stage "+string(j.State)
		if kind == "" {
			kind = domain.HumanDecisionRequired
		}
		if s.Reason != "" {
			detail = workflow.RenderReason(s.Reason, j.TaskID, string(j.State), j.stagedAttempt())
		}
		stop(&j, kind, detail, &events)
		return j, nil, events, nil
	}
	if s.Primitive == workflow.ExternalEffect && (j.ExecutionKind != "live" || (j.Disposition.Mode != domain.Normal && !s.IsGitPush())) {
		stop(&j, domain.ExternalEffectRefused, "UnsupportedExternalEffect: requires the trusted runtime and a Normal disposition", &events)
		return j, nil, events, nil
	}
	if s.Primitive == workflow.HumanDecision {
		return awaitDecision(j, events)
	}
	if s.Primitive == workflow.ChangeRequest {
		j.Counts[string(j.State)]++
		if o.ValidateChangeRequest != nil {
			if err := o.ValidateChangeRequest(j, s); err != nil {
				if err = routeFailure(&j, "change_request", err.Error(), "", &events); err != nil {
					return input, nil, nil, err
				}
				return j, nil, events, nil
			}
		}
		if err := changeRequestStep(&j, s, &events); err != nil {
			return input, nil, nil, err
		}
		return j, nil, events, nil
	}
	if s.Primitive == workflow.SubWorkflow {
		return awaitChild(j, s, events)
	}
	if s.Primitive == workflow.FanOut || s.Primitive == workflow.FanIn {
		if err := fanStep(&j, s, &events); err != nil {
			reason := "width"
			var fanErr *FanOutValidationError
			if errors.As(err, &fanErr) && fanErr.Code != "" {
				reason = fanErr.Code
			}
			if err = routeFailure(&j, reason, err.Error(), "", &events); err != nil {
				return input, nil, nil, err
			}
		}
		return j, nil, events, nil
	}
	var next string
	var err error
	switch s.Primitive {
	case workflow.Gate:
		j.Counts[string(j.State)]++
		g := record(&j, workflow.RecordGate, string(j.State), s.Name, 0, *s.Expr, current(j), "gate", &events)
		if !g.Passed {
			if n := sameGate(&j, string(j.State), g); n > SameGateRepeats {
				stop(&j, domain.SameGate, fmt.Sprintf("%s failed %d times for the same reason", j.State, n), &events)
				return j, nil, events, nil
			}
			if s.Failure != "" {
				if err := routeFailure(&j, "gate", fmt.Sprintf("gate %s returned %s", j.State, g.Value), "", &events); err != nil {
					return input, nil, nil, err
				}
				return j, nil, events, nil
			}
			next = s.Failure
		} else {
			sameGate(&j, string(j.State), g)
			next, err = route(j, s)
		}
	case workflow.Branch:
		next = s.Default
		for i, arm := range s.Cases {
			g := record(&j, workflow.RecordBranchArm, string(j.State), "", i+1, arm.When, branchEvidence(j), "branch-arm", &events)
			if g.Value == workflow.Unknown {
				stop(&j, domain.HumanDecisionRequired, fmt.Sprintf("UnknownDecision: %s case %d is unknown", j.State, i), &events)
				return j, nil, events, nil
			}
			if g.Passed {
				next = arm.Next
				break
			}
		}
	case workflow.Loop:
		stopped, discounted := loopProgress(&j, s, &events)
		if stopped {
			return j, nil, events, nil
		}
		if !discounted && j.Counts[string(j.State)] >= s.MaxIterations {
			kind := domain.LoopCap
			for _, other := range j.Document.Stages {
				// Exhausting an Integrate stage's remediation route is an integration conflict.
				if other.Primitive == workflow.Integrate && other.Reverify == string(j.State) {
					kind = domain.IntegrationConflict
				}
			}
			detail := fmt.Sprintf("%s exhausted %d iterations", j.State, s.MaxIterations)
			if s.ExitGate != "" {
				detail += " without passing exit gate " + s.ExitGate
			}
			// A resumed or restarted task re-enters its loops on the
			// counts it carries (FAE-WF-074); the detail says so.
			detail += fmt.Sprintf(": %d of %d used; the iteration count carries for the life of the task, across resume and restart", j.Counts[string(j.State)], s.MaxIterations)
			if s.OnExhaustion != "" {
				kind = s.OnExhaustion
			}
			stop(&j, kind, detail, &events)
			return j, nil, events, nil
		}
		if s.BackoffSeconds > 0 && j.Counts[string(j.State)] > 0 && !backedOff {
			return backoff(j, s, events)
		}
		if !discounted {
			j.Counts[string(j.State)]++
		}
		next, err = route(j, s)
	default:
		selected := map[string]workflow.Binding{}
		if s.Primitive == workflow.AgentTurn || s.Primitive == workflow.Review {
			roles := []string{s.Role}
			if s.Primitive == workflow.Review && len(s.Reviewers) > 0 {
				roles = s.Reviewers
			}
			for _, name := range roles {
				declared := j.roleRoutes(name)
				// A binding the bridge refused is never selected again in
				// its route generation (FAE-RT-010, FAE-WF-082).
				routes := j.unrefusedRoutes(declared, o.RouteGeneration)
				refusedAll := len(declared) > 0 && len(routes) == 0
				var route workflow.Binding
				ok := len(routes) > 0
				if ok {
					route = routes[0]
				}
				if selectRoute != nil && !refusedAll {
					route, ok = selectRoute(routes)
				}
				if !ok {
					if o.QuotaExhausted != nil && !refusedAll {
						if resetAt, quota := o.QuotaExhausted(routes); quota {
							resetAt = resetAt.UTC()
							if j.RoutesUnavailableSince.IsZero() || j.RoutesUnavailableSince.Before(resetAt) {
								j.RoutesUnavailableSince = resetAt
							}
							j.QuotaResetAt = resetAt
							j.Parked = true
							j.Reason = ParkProviderQuota
							if !input.Parked || utf8StoredText(input.Reason) != utf8StoredText(j.Reason) {
								event(&j, Event{Kind: "park", Stage: string(j.State), Attempt: j.stagedAttempt(), Cause: j.Reason}, &events)
							}
							return j, nil, events, nil
						}
					}
					// No eligible route, unhealthy or refused: the task
					// parks without an attempt and escalates only past the
					// route-exhaustion bound (FAE-RT-010, Appendix B). The
					// clock runs from the first such park until a route
					// runs (complete clears it), so a retry in a new route
					// generation that the bridge refuses again keeps it: a
					// refusal that recurs at every daemon start still
					// escalates once the bound has passed since the first
					// park.
					now := time.Now().UTC()
					if j.RoutesUnavailableSince.IsZero() {
						j.RoutesUnavailableSince = now
					}
					if now.Sub(j.RoutesUnavailableSince) >= o.RouteExhaustion {
						detail := fmt.Sprintf("no eligible route for role %s since %s", name, j.RoutesUnavailableSince.Format(time.RFC3339))
						if refused := j.routeRefusalsDetail(declared, o.RouteGeneration); refused != "" {
							detail += "; refused by the bridge: " + refused
						}
						if refusedAll {
							detail += "; every route was refused before its provider ran: " + routesRefusedRemedy + ", restart the daemon, and restart the task with information"
						}
						// The escalation keeps the clock, which dates the
						// episode for the daemon's route-exhaustion alert;
						// restart with information clears it.
						stop(&j, domain.ProviderUnavailable, detail, &events)
						return j, nil, events, nil
					}
					j.Parked = true
					j.Reason = ParkRoutesUnavailable
					if refusedAll {
						j.Reason = routesRefusedReason(j, name, declared, o.RouteGeneration)
					}
					// The same park decided again records nothing. The
					// stored reason is compared as the plane stores text,
					// so a reason that differs only by that normalisation
					// is the same park.
					if !input.Parked || utf8StoredText(input.Reason) != utf8StoredText(j.Reason) {
						event(&j, Event{Kind: "park", Stage: string(j.State), Attempt: j.stagedAttempt(), Cause: j.Reason}, &events)
					}
					return j, nil, events, nil
				}
				selected[name] = route
			}
			j.QuotaResetAt = time.Time{}
			if n, bound := j.Failures[turnStartsKeyPrefix+string(j.State)], StageAgentStartBound(j, string(j.State)); n >= bound {
				stopStageAgentStarts(&j, string(j.State), n, bound, &events)
				return j, nil, events, nil
			}
		}
		attemptBoundary := j.NewAttempt || j.Attempts == 0 || j.Counts[string(j.State)] > 0
		if j.BranchContext != nil && j.BranchAttemptPaid {
			attemptBoundary = false
			j.BranchAttemptPaid = false
		}
		// Readiness observations are not new implementation attempts. A
		// separate durable observation budget bounds their waiting loop.
		operation := j.ReviewStages[string(j.State)]
		if j.Attempts > 0 && (operation == "review_read" || operation == "review_publish" || operation == "review_merge") {
			attemptBoundary = false
		}
		// The per-task test-run budget is checked before the attempt limit:
		// a task out of both is TestRunCap, which no raise of the attempt
		// limit resumes (FAE-WF-074).
		if s.TestRun && j.TestRuns >= j.Document.Limits.TestRuns {
			stop(&j, domain.TestRunCap, fmt.Sprintf("%s needs a test run: %d of %d test runs used; the test-run count carries for the life of the task, across resume and restart", j.State, j.TestRuns, j.Document.Limits.TestRuns), &events)
			return j, nil, events, nil
		}
		if attemptBoundary && j.Attempts >= j.AttemptLimit() {
			stop(&j, domain.AttemptCap, "", &events)
			return j, nil, events, nil
		}
		if s.Primitive == workflow.VcsCommit || s.Primitive == workflow.Integrate || s.Primitive == workflow.ExternalEffect {
			if e := protected(j); e != nil {
				// A stale verdict re-runs through the stage's declared
				// re-verification route (FAE-ROB-013); without one, resume
				// cannot re-validate it.
				if s.Reverify == "" {
					stop(&j, domain.ResumeEvidence, "ProtectedEvidence: "+e.Error(), &events)
					return j, nil, events, nil
				}
				event(&j, Event{Kind: "evidence-stale", Stage: string(j.State), Attempt: j.stagedAttempt(), Cause: "re-validate: " + e.Error()}, &events)
				if err := move(&j, s.Reverify, "stale protected evidence re-validates", &events); err != nil {
					return input, nil, nil, err
				}
				return j, nil, events, nil
			}
		}
		argv, bindErr := dispatchArgv(s, boundInputs(j, s))
		var templateValues map[string]any
		if workflow.HasRuntimeReferences(s.Command) {
			templateValues, bindErr = commandTemplateValues(j, s)
			if bindErr == nil {
				argv, bindErr = BindStageCommand(j, s)
			}
		}
		if bindErr != nil {
			j.Counts[string(j.State)]++
			if s.Failure != "" || workflow.BranchFailureJoin(j.Document, string(j.State)) != "" {
				if err := routeFailure(&j, "binding", bindErr.Error(), "", &events); err != nil {
					return input, nil, nil, err
				}
				return j, nil, events, nil
			}
			stop(&j, domain.HumanDecisionRequired, "CommandBinding: "+bindErr.Error(), &events)
			return j, nil, events, nil
		}
		// A dispatch keeps the route-exhaustion clock: it clears only when
		// the dispatch's provider runs (complete).
		// One initial attempt covers first visits to the graph. Each repeated
		// execution consumes another attempt; Sequence counts every dispatch.
		if attemptBoundary {
			j.NewAttempt = false
			j.Attempts++
		}
		j.Sequence++
		j.Counts[string(j.State)]++
		if s.TestRun {
			j.TestRuns++
		}
		d := Dispatch{OutputSchema: append(json.RawMessage(nil), j.Document.Artifacts[s.Output].Schema...), TemplateValues: templateValues, PriorRejection: append([]string(nil), j.PriorRejections[string(j.State)]...), ExecutionHead: j.ExpectedHead(), OperatorContext: j.LaterContext(), AttemptCharged: attemptBoundary, Base: j.Base, ExecutionKind: j.ExecutionKind, ID: fmt.Sprintf("%s/%s/%d", j.TaskID, j.WorkflowHash, j.Sequence), TaskID: j.TaskID, Stage: string(j.State), Tree: j.Tree, Attempt: j.Attempts, Visit: j.Counts[string(j.State)], Config: s, Inputs: boundInputs(j, s), CreatedAt: time.Now().UTC(), Argv: argv, TaskClass: j.TaskClass, Scope: j.ApprovedScope, FollowUpOf: j.FollowUpOf, TurnBudget: j.TurnBudget}
		if err := bindRunStats(&j, &d); err != nil {
			return input, nil, nil, err
		}
		if j.BranchContext != nil {
			d.BranchInstance = j.TaskID + "/" + branchInstanceKey(j.BranchContext)
			d.BranchID = j.BranchContext.Path
			d.FanStage = j.BranchContext.Fan
			d.BranchItem = j.BranchContext.Item
			d.WriteScope = append([]string(nil), j.BranchContext.WriteScope...)
			if n := len(j.BranchContext.IndexPath); n > 0 {
				index := j.BranchContext.IndexPath[n-1]
				d.BranchIndex = &index
			}
		}
		if j.Fan != nil {
			if _, b := j.Fan.activeBranch(); b != nil {
				d.BranchID = b.ID
				d.FanStage = j.Fan.Stage
				d.BranchItem = b.Item
				d.WriteScope = append([]string(nil), b.WriteScope...)
			}
		}
		// BindDispatchReferences hook: branch identity/item is final here.
		if err := BindDispatchReferences(j, &d); err != nil {
			if routeErr := routeFailure(&j, "binding", err.Error(), d.ID, &events); routeErr != nil {
				return input, nil, nil, routeErr
			}
			return j, nil, events, nil
		}
		if len(d.TemplatePresence) > 0 {
			presence := map[string]bool{}
			for key, value := range d.TemplatePresence {
				presence[key] = value
			}
			event(&j, Event{Kind: "template-presence", Stage: d.Stage, DispatchID: d.ID, Visit: d.Visit, BranchID: d.BranchID, TemplatePresence: presence}, &events)
		}

		for i := range j.Notifications {
			if j.Notifications[i].DeliveredAt.IsZero() {
				j.Notifications[i].DeliveredAt = d.CreatedAt
				d.Notifications = append(d.Notifications, j.Notifications[i])
			}
		}
		if len(d.Notifications) > 0 {
			quoted := formatNotificationsQuoted(d.Notifications)
			if d.OperatorContext != "" {
				d.OperatorContext += "\n" + quoted
			} else {
				d.OperatorContext = quoted
			}
		}
		roles := []string{s.Role}
		if s.Primitive == workflow.Review && len(s.Reviewers) > 0 {
			roles = s.Reviewers
		}
		count := 1
		if s.Primitive == workflow.Review && s.Count > 0 {
			count = s.Count
		}
		for _, r := range roles {
			for i := 0; i < count; i++ {
				route := selected[r]
				cred := ""
				if o.CredentialFor != nil && route.Provider != "" {
					cred = o.CredentialFor(route)
				}
				d.Sessions = append(d.Sessions, Session{
					ID:                 fmt.Sprintf("%s/session/%d", d.ID, len(d.Sessions)),
					Role:               r,
					Route:              route,
					TaskID:             j.TaskID,
					Priority:           o.Priority,
					Admitted:           o.Admitted,
					CutOffByQuota:      j.CutOffByQuota,
					CredentialIdentity: cred,
					ResumeSessionID:    j.InterruptedSessionID,
				})
			}
		}
		if len(selected) > 0 {
			d.RouteGeneration = o.RouteGeneration
		}
		if cp, ok := j.CheckpointForStage(d.Stage); ok {
			d.ResumeCheckpoint = &cp
			for i := range d.Sessions {
				roleSpec, _ := workflow.EffectiveRole(j.Document, j.TaskClass, d.Sessions[i].Role)
				if roleSpec.Fresh {
					d.Sessions[i].ResumeSessionID = ""
				} else if d.Sessions[i].ResumeSessionID == "" {
					d.Sessions[i].ResumeSessionID = cp.ProviderSession
				}
			}
		} else {
			for i := range d.Sessions {
				roleSpec, _ := workflow.EffectiveRole(j.Document, j.TaskClass, d.Sessions[i].Role)
				if roleSpec.Fresh {
					d.Sessions[i].ResumeSessionID = ""
				}
			}
		}
		j.InFlight = &d
		if j.Fan != nil {
			if _, b := j.Fan.activeBranch(); b != nil {
				b.InFlight = &d
				b.Stage = d.Stage
			}
		}
		event(&j, Event{Kind: "dispatch", Stage: d.Stage, Attempt: d.Attempt, Cause: "inputs-resolved", DispatchID: d.ID}, &events)
		return j, &d, events, nil
	}
	if err != nil {
		return input, nil, nil, err
	}
	if err = move(&j, next, "stage-complete", &events); err != nil {
		return input, nil, nil, err
	}
	return j, nil, events, nil
}
func protected(j Job) error {
	var runs, reviews, changes []workflow.Evidence
	order := map[string]int{}
	for i, event := range j.History {
		if event.Kind == "dispatch" {
			order[event.DispatchID] = i + 1
		}
	}
	position := func(a workflow.Evidence) int {
		n := 0
		for _, p := range j.Provenance {
			if p.Stage == a.Stage && p.ArtifactID == a.ID && p.Session == a.Session && order[p.DispatchID] > n {
				n = order[p.DispatchID]
			}
		}
		return n
	}
	lastChange := 0
	for _, a := range latestInputs(j) {
		if workflow.ProducesChanges(j.Document, j.Document.Stages[a.Stage]) {
			lastChange = max(lastChange, position(a))
			changes = append(changes, a)
		}
	}
	for _, e := range j.History {
		if e.Kind == "tree-reconciled" || e.Kind == "integration-target-moved" {
			lastChange = max(lastChange, order[e.DispatchID])
		}
	}
	for _, a := range current(j) {
		s := j.Document.Stages[a.Stage]
		if s.Primitive == workflow.HostCommand && s.TestRun && position(a) > lastChange {
			runs = append(runs, a)
		} else if s.Primitive == workflow.HostCommand && s.Check != nil && position(a) > lastChange {
			if count, ok := passingCheckRunCount(s.Check, a.Fields); ok {
				synth := a
				fields := map[string]any{}
				for k, v := range a.Fields {
					fields[k] = v
				}
				fields["exit_code"] = 0
				fields["executed_count"] = count
				synth.Fields = fields
				runs = append(runs, synth)
			}
		}
		if s.Primitive == workflow.Review && position(a) > lastChange {
			reviews = append(reviews, a)
		}
	}
	docChecksSatisfied := false
	if docCount, ok := j.passingDocumentChecksCount(); ok {
		docChecksSatisfied = true
		if len(runs) == 0 {
			runs = append(runs, workflow.Evidence{
				ID:         "document-checks/" + j.Tree,
				TaskID:     j.TaskID,
				Type:       "DocumentChecks",
				Version:    1,
				Stage:      "document_checks",
				Tree:       j.Tree,
				Provenance: true,
				Fields:     map[string]any{"exit_code": 0, "executed_count": docCount},
			})
		}
	}
	reviews = append(reviews, carriedReviews(j, position)...)
	if e := workflow.CheckProtectedEvidence(j.Tree, runs, reviews, changes); e != nil {
		return e
	}
	if docChecksSatisfied {
		return nil
	}
	// A passing gate must actually cite the current-tree run used as evidence.
	for i := len(j.History) - 1; i >= 0; i-- {
		e := j.History[i]
		if e.Gate == nil || !e.Gate.Passed {
			continue
		}
		stage := j.Document.Stages[e.Stage]
		for _, r := range runs {
			if stage.TestStage != "" && r.Stage != stage.TestStage {
				continue
			}
			if stage.TestStage == "" && j.Document.Stages[r.Stage].Check == nil {
				continue
			}
			for _, id := range e.Gate.EvidenceIDs {
				if r.ID == id {
					return nil
				}
			}
		}
	}
	return errors.New("no recorded passing test gate")
}

func (j Job) passingDocumentChecksCount() (int, bool) {
	if len(j.DeclaredFiles) == 0 || len(j.DocumentGlobs) == 0 || len(j.DocumentChecks) == 0 {
		return 0, false
	}
	for _, f := range j.DeclaredFiles {
		if !workflow.MatchesAnyGlob(f, j.DocumentGlobs) {
			return 0, false
		}
	}
	total := 0
	for _, dc := range j.DocumentChecks {
		if dc.ParseError != "" || dc.ExitCode != 0 || dc.CountField == "" || dc.Count < 1 {
			return 0, false
		}
		total += dc.Count
	}
	return total, total >= 1
}

func passingCheckRunCount(spec *workflow.CheckSpec, fields map[string]any) (int, bool) {
	if spec == nil || spec.CountField == "" || fields == nil {
		return 0, false
	}
	if exitVal, hasExit := fields["exit_code"]; hasExit {
		n, ok := numericValue(exitVal)
		if !ok || n != 0 {
			return 0, false
		}
	}
	val, ok := lookupNestedField(fields, spec.CountField)
	if !ok {
		return 0, false
	}
	n, ok := numericValue(val)
	if !ok || n < 1 {
		return 0, false
	}
	return int(n), true
}

func numericValue(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func validateCompletedCheckOutput(doc workflow.Document, stageID string, s workflow.Stage, fields map[string]any) error {
	if s.Check == nil {
		return nil
	}
	val, ok := lookupNestedField(fields, s.Check.CountField)
	if !ok {
		return fmt.Errorf("MalformedOutput: check output missing count_field %s", s.Check.CountField)
	}
	if _, ok := numericValue(val); !ok {
		return fmt.Errorf("MalformedOutput: check count_field %s must be numeric", s.Check.CountField)
	}
	if exitVal, hasExit := fields["exit_code"]; hasExit {
		exitCode, ok := numericValue(exitVal)
		if !ok {
			return errors.New("MalformedOutput: check exit_code must be numeric")
		}
		_ = exitCode // A parsed check is evidence; gates decide its exit status.
	}
	return nil
}

func stageRoutesOnExitCode(doc workflow.Document, stageID, outputType string) bool {
	var exprRefsExitCode func(workflow.Expr) bool
	exprRefsExitCode = func(x workflow.Expr) bool {
		if x.Op == "field" && len(x.Path) == 1 && x.Path[0] == "exit_code" && (x.Stage == stageID || (outputType != "" && x.Type == outputType)) {
			return true
		}
		for _, a := range x.Args {
			if exprRefsExitCode(a) {
				return true
			}
		}
		return false
	}
	for _, e := range doc.Edges {
		if e.From != stageID {
			continue
		}
		target, ok := doc.Stages[e.To]
		if !ok {
			continue
		}
		if target.Expr != nil && exprRefsExitCode(*target.Expr) {
			return true
		}
		for _, arm := range target.Cases {
			if exprRefsExitCode(arm.When) {
				return true
			}
		}
	}
	return false
}

// Complete only accepts the currently persisted dispatch and its assigned
// sessions. Envelope provenance is created by the executor, never role JSON.
func complete(input Job, dispatchID string, r Result) (Job, []Event, error) {
	j := clone(input)
	events := []Event{}
	if e := Validate(j); e != nil {
		return input, nil, e
	}
	match := j.InFlight != nil && j.InFlight.ID == dispatchID && r.DispatchID == dispatchID
	validEnvelope := j.InFlight != nil && len(j.InFlight.Sessions) > 0 && r.TaskID == j.TaskID && r.Tree == j.InFlight.Tree && !r.ProducedAt.Before(j.InFlight.CreatedAt) && r.Session == j.InFlight.Sessions[0].ID
	if !kernel.CanComplete(j.State.Terminal() || j.State == domain.Escalated, j.InFlight != nil, match, validEnvelope) {
		return input, nil, errors.New("stale or terminal completion")
	}
	d := *j.InFlight
	if r.TaskID != j.TaskID || r.Tree != d.Tree || r.ProducedAt.Before(d.CreatedAt) || r.Session != d.Sessions[0].ID {
		return input, nil, errors.New("completion provenance mismatch")
	}
	recordTurnStats(&j, d, r, &events)
	if !routeNeverLaunched(r) {
		j.RoutesUnavailableSince = time.Time{}
	}
	if r.Detail != "" {
		detail := r.Detail
		if len(detail) > 2048 {
			detail = detail[:2048]
		}
		event(&j, Event{Kind: "execution-diagnostic", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: detail}, &events)
	}
	if r.Checkpoint != nil && r.Checkpoint.ProviderSession != "" {
		j.SaveCheckpoint(d.Stage, *r.Checkpoint)
		if r.Class == ClassCheckpointInterrupted || r.Class == "interrupted" {
			refund(&j, d)
			j.InFlight = nil
			j.Parked = false
			j.Reason = ""
			event(&j, Event{Kind: "checkpoint-resume", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "resume from durable checkpoint after interruption"}, &events)
			return j, events, nil
		}
	}
	if r.Class == "uncertain" || r.Class == "interrupted" || r.Class == ClassCheckpointInterrupted {
		j.Parked = true
		j.Reason = "UncertainDispatch"
		event(&j, Event{Kind: "park", Stage: d.Stage, Attempt: d.Attempt, Cause: j.Reason, DispatchID: d.ID}, &events)
		return j, events, nil
	}
	if r.Class == ClassTurnBudgetExhausted {
		cause := bounded(r.Detail)
		if cause == "" {
			cause = ClassTurnBudgetExhausted
		}
		event(&j, Event{Kind: ClassTurnBudgetExhausted, Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: cause}, &events)
		role, _ := workflow.EffectiveRole(j.Document, j.TaskClass, d.Config.Role)
		onExhaustion := ""
		if budget := workflow.EffectiveTurnBudget(role, d.Config); budget != nil {
			onExhaustion = budget.OnExhaustion
		}
		resumeKey := "turn-budget-resume:" + d.Stage
		switch {
		case onExhaustion == "resume-once":
			if j.Failures[resumeKey] == 0 {
				j.Failures[resumeKey]++
				refund(&j, d)
				j.InFlight = nil
				event(&j, Event{Kind: "turn-budget-resume", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "resume-once from durable checkpoint"}, &events)
				return j, events, nil
			}
			delete(j.Failures, resumeKey)
			delete(j.TurnCheckpoints, d.Stage)
			if err := routeFailure(&j, "budget", cause, d.ID, &events); err != nil {
				return input, nil, err
			}
			return j, events, nil
		case onExhaustion != "" && onExhaustion != "fail":
			delete(j.Failures, resumeKey)
			delete(j.TurnCheckpoints, d.Stage)
			if err := routeFailureTo(&j, onExhaustion, "budget", cause, d.ID, &events); err != nil {
				return input, nil, err
			}
			return j, events, nil
		default:
			delete(j.Failures, resumeKey)
			delete(j.TurnCheckpoints, d.Stage)
			if err := routeFailure(&j, "budget", cause, d.ID, &events); err != nil {
				return input, nil, err
			}
			return j, events, nil
		}
	}
	if r.Class == "startup-retry" || r.Class == ClassLaterReviewerNotStarted {
		return completeStartupRetry(input, j, d, r, events)
	}
	if r.Class == "stall-retry" {
		if j.ExecutionKind != "live" || r.ExecutionKind != "live" || (d.Config.Primitive != workflow.AgentTurn && d.Config.Primitive != workflow.Review) || r.NewTree != "" || r.NewBase != "" {
			return input, nil, errors.New("invalid stall recovery")
		}
		// The durable per-task counter (FAE-ROB-017) counts the stalls of
		// the current turn; any end of the turn clears it, this escalation
		// included (enter).
		j.Failures[StallRecoveryKey]++
		j.InFlight = nil
		if n := j.Failures[StallRecoveryKey]; n > StallRetries {
			stop(&j, domain.StallExhausted, fmt.Sprintf("%s stalled %d times in one turn: %d of %d retries used; the bound is per turn, so the next turn starts a fresh one", d.Stage, n, n-1, StallRetries), &events)
			return j, events, nil
		}
		event(&j, Event{Kind: "stall-retry", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "retired trusted bridge; unchanged tree"}, &events)
		return j, events, nil
	}
	if r.Class == "target-moved" {
		if j.ExecutionKind != "live" || r.ExecutionKind != "live" || d.Config.Primitive != workflow.Integrate || d.Config.Reverify == "" || r.NewTree != "" || r.NewBase != "" {
			return input, nil, errors.New("integration retry outside pinned live bounded route")
		}
		j.InFlight = nil
		event(&j, Event{Kind: "integration-target-moved", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "known no-write refusal; full reconciliation required"}, &events)
		if err := move(&j, d.Config.Reverify, "integration target moved", &events); err != nil {
			return input, nil, err
		}
		return j, events, nil
	}
	if r.Class == "infrastructure-deferred" && j.ExecutionKind == "live" {
		// Host admission deferred a session before it launched (FAE-HOST-006):
		// nothing of it ran, so nothing is charged. A later review session's
		// deferral carries the reviews that ran before it; they are not
		// recorded, and the next dispatch runs every reviewer again
		// (FAE-RT-004, FAE-WF-033).
		if len(r.Reviews) != 0 && (d.Config.Primitive != workflow.Review || len(r.Output) != 0 || r.NewTree != "" || r.NewBase != "" || !reviewsBeforeFailure(d, r.Reviews)) {
			return input, nil, errors.New("invalid admission deferral")
		}
		refund(&j, d)
		j.InFlight = nil
		// Host pressure is not provider failure, even after earlier isolated
		// reviewers completed. Keep the diagnostic count without converting it
		// into an escalation. A partial batch cannot stand for a Review stage;
		// the next attempt uses fresh independent sessions (FAE-WF-033).
		if len(r.Reviews) != 0 {
			j.Failures[turnDeferralKeyPrefix+d.Stage]++
		}

		j.Parked = true
		j.Reason = "infrastructure-deferred"
		if !strings.Contains(r.Detail, "provider pool") {
			j.AdmissionRetryAfter = time.Now().Add(5 * time.Second)
		} else {
			j.AdmissionRetryAfter = time.Time{}
		}
		event(&j, Event{Kind: "park", Stage: d.Stage, Attempt: d.Attempt, Cause: j.Reason, DispatchID: d.ID}, &events)
		return j, events, nil
	}
	if r.Class == ClassStaleResidue || r.Class == ClassRejectedChange || r.Class == ClassOwnHunkLoss {
		if j.ExecutionKind != "live" || r.ExecutionKind != "live" || len(r.Output) != 0 || (len(r.Reviews) != 0 && (r.Class != ClassRejectedChange || d.Config.Primitive != workflow.Review || !reviewsBeforeFailure(d, r.Reviews))) || r.NewTree != "" || r.NewBase != "" || (r.Class == ClassOwnHunkLoss && d.Config.Primitive != workflow.Merge && d.Config.Primitive != workflow.VcsCommit && !workflow.ProducesChanges(j.Document, d.Config)) {
			return input, nil, errors.New("invalid typed refusal")
		}
		kind := domain.StaleBranchResidue
		if r.Class == ClassOwnHunkLoss {
			kind = domain.MergeOwnHunkLoss
		}
		if r.Class == ClassStaleResidue || r.Class == ClassOwnHunkLoss {
			// Nothing launched or no write landed: the refused turn charges no budget.
			refund(&j, d)
		}
		stop(&j, kind, bounded(r.Detail), &events)
		return j, events, nil
	}
	if out, events, ok, err := commitRefused(input, j, d, r, events); ok {
		return out, events, err
	}
	if out, events, ok, err := vcsRefused(input, j, d, r, events); ok {
		return out, events, err
	}
	if out, events, ok, err := effectRefused(input, j, d, r, events); ok {
		return out, events, err
	}
	if r.Class == ClassBindingRefused {
		return completeBindingRefused(input, j, d, r, events)
	}
	if r.Class == ClassRefusedBeforeLaunch && j.ExecutionKind == "live" {
		return completeRefusedBeforeLaunch(input, j, d, r, events)
	}
	if r.Class == ClassQuotaCutoff {
		if (d.Config.Primitive != workflow.AgentTurn && d.Config.Primitive != workflow.Review) || r.NewBase != "" {
			return input, nil, errors.New("invalid quota cutoff")
		}
		if r.NewTree != "" {
			if j.ExecutionKind != "live" || !workflow.ProducesChanges(j.Document, d.Config) {
				return input, nil, errors.New("quota cutoff tree change outside change producer")
			}
			j.Tree = r.NewTree
		}
		refund(&j, d)
		clearTurnBounds(&j)
		j.InFlight = nil
		j.Parked = true
		j.Reason = ParkProviderQuota
		j.CutOffByQuota = true
		if r.InterruptedSession != "" {
			j.InterruptedSessionID = r.InterruptedSession
		}
		cpSession := r.InterruptedSession
		if r.Checkpoint != nil && r.Checkpoint.ProviderSession != "" {
			cpSession = r.Checkpoint.ProviderSession
			if j.InterruptedSessionID == "" {
				j.InterruptedSessionID = cpSession
			}
		}
		cpTree := j.Tree
		if r.NewTree != "" {
			cpTree = r.NewTree
		}
		if r.Checkpoint != nil && r.Checkpoint.WorktreeTree != "" {
			cpTree = r.Checkpoint.WorktreeTree
		}
		if cpSession != "" {
			cp := TurnCheckpoint{
				TaskID:          j.TaskID,
				StageID:         d.Stage,
				Visit:           d.Visit,
				Attempt:         d.Attempt,
				ProviderSession: cpSession,
				WorktreeTree:    cpTree,
				UpdatedAt:       time.Now().UTC(),
			}
			if r.Checkpoint != nil && r.Checkpoint.AssistantTurns > 0 {
				cp.AssistantTurns = r.Checkpoint.AssistantTurns
			}
			j.SaveCheckpoint(d.Stage, cp)
		}
		if !r.QuotaResetAt.IsZero() {
			resetAt := r.QuotaResetAt.UTC()
			j.QuotaResetAt = resetAt
			if j.RoutesUnavailableSince.IsZero() || j.RoutesUnavailableSince.Before(resetAt) {
				j.RoutesUnavailableSince = resetAt
			}
		}
		event(&j, Event{Kind: "park", Stage: d.Stage, Attempt: j.stagedAttempt(), Cause: ParkProviderQuota, DispatchID: d.ID}, &events)
		return j, events, Validate(j)
	}
	if r.Class == ClassProviderFailover {
		if j.ExecutionKind != "live" || r.ExecutionKind != "live" || (d.Config.Primitive != workflow.AgentTurn && d.Config.Primitive != workflow.Review) || len(r.Output) != 0 || r.NewTree != "" || r.NewBase != "" || !reviewsBeforeFailure(d, r.Reviews) {
			return input, nil, errors.New("invalid provider failover")
		}
		// FAE-RT-010/ROB-017: one rotation per turn; the next selection
		// skips the binding the failure made ineligible. It charges no
		// attempt, unless earlier review sessions of the dispatch ran: then
		// the dispatch stays charged and the stage runs every reviewer again
		// (FAE-RT-004, FAE-WF-033).
		key := turnFailoverKeyPrefix + d.Stage
		if j.Failures[key] < FailoverRotations {
			j.Failures[key]++
			if len(r.Reviews) == 0 {
				refund(&j, d)
			}
			// The next route gets its full agent-start bound, as after a
			// refusal (FAE-RT-004). The rotation stays spent.
			clearStageAgentStarts(&j, d.Stage)
			j.InFlight = nil
			event(&j, Event{Kind: "failover", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: bounded(r.Detail) + reviewsRanCause(d, r.Reviews, len(r.Reviews) != 0)}, &events)
			return j, events, nil
		}
		// The recorded failure ends the turn and clears its rotation (failed).
		return failed(j, "ProviderFailure: "+bounded(r.Detail))
	}
	if (r.Class == ClassMalformedOutput || r.Class == ClassValidationFailed) && j.ExecutionKind == "live" {
		if r.ExecutionKind != "live" || r.NewTree != "" || r.NewBase != "" {
			return input, nil, errors.New("invalid bounded validation failure")
		}
		prefix := "MalformedOutput: "
		if r.Class == ClassValidationFailed {
			prefix = "ValidationFailed: "
		}
		return failed(j, prefix+bounded(r.Detail))
	}
	if (r.Class == "review-wait" || r.Class == "review-gate-failed") && j.ExecutionKind == "live" && j.ReviewStages[d.Stage] != "" {
		if r.Class == "review-gate-failed" {
			stop(&j, domain.ReviewGateFailed, r.Detail, &events)
			return j, events, nil
		}
		parkReviewPoll(&j, d, r.Detail, &events)
		return j, events, nil
	}
	if r.Class != "" && r.Class != "success" {
		return failed(j, "ExecutionFailure: "+r.Class)
	}
	// A completed execution ends the turn: its agent-start, failover and
	// stall bounds are per turn, so a later turn gets fresh ones.
	clearTurnBounds(&j)
	s := d.Config
	if j.ExecutionKind == "live" && r.ExecutionKind != "live" {
		return input, nil, errors.New("live completion kind required")
	}
	if j.ExecutionKind == "scripted" && (r.ExecutionKind == "live" || r.NewTree != "") {
		return input, nil, errors.New("scripted completion cannot claim live tree")
	}
	if r.NewBase != "" && (j.ExecutionKind != "live" || s.Primitive != workflow.Merge || s.Reverify == "") {
		return input, nil, errors.New("base mutation outside reconciliation")
	}
	resultTree := d.Tree
	if r.NewTree != "" {
		if j.ExecutionKind != "live" || !(((s.Primitive == workflow.AgentTurn || s.Primitive == workflow.Review) && workflow.ProducesChanges(j.Document, s)) || (s.Primitive == workflow.HostCommand && s.Generate != nil) || (s.Primitive == workflow.Merge && s.Reverify != "")) {
			return input, nil, errors.New("tree mutation outside change-producing agent turn")
		}
		resultTree = r.NewTree
	}
	// A successful merge proves only the reviewed tree. Any subsequent source
	// edit or base reconciliation invalidates that proof before Done can close
	// the task (and its ticket) using stale delivery evidence.
	if resultTree != j.Tree {
		j.ReviewMerged = false
	}
	outputs := []ReviewResult{{Session: r.Session, Role: s.Role, Output: r.Output, Transcript: r.Transcript}}
	if s.Primitive == workflow.Review {
		if len(r.Reviews) != len(d.Sessions) {
			return failed(j, "MalformedOutput: review count")
		}
		outputs = r.Reviews
		for i, o := range outputs {
			if o.Session != d.Sessions[i].ID || o.Role != d.Sessions[i].Role {
				return input, nil, errors.New("review session mismatch")
			}
		}
	}
	// Validate all outputs before recording any, so partial reviewer collections
	// cannot survive as an aggregate approval.
	records := []workflow.Evidence{}
	transcripts := map[string]*Transcript{}
	for _, o := range outputs {
		transcripts[o.Session] = o.Transcript
		if s.Output == "" {
			return failed(j, "MalformedOutput: stage has no output type")
		}
		typ := j.Document.Artifacts[s.Output]
		if e := workflow.ValidateArtifact(typ.Schema, o.Output); e != nil {
			return failed(j, "MalformedOutput: "+e.Error())
		}
		var fields map[string]any
		dec := json.NewDecoder(bytes.NewReader(o.Output))
		dec.UseNumber()
		if e := dec.Decode(&fields); e != nil || fields == nil {
			return failed(j, "MalformedOutput: object required")
		}
		if s.Primitive == workflow.HostCommand && s.TestRun {
			if fields["fresh_build"] != true {
				return failed(j, "MalformedOutput: fresh build required")
			}
		}
		if s.Primitive == workflow.HostCommand && s.Check != nil {
			if checkErr := validateCompletedCheckOutput(j.Document, d.Stage, s, fields); checkErr != nil {
				return failed(j, checkErr.Error())
			}
		}
		id, e := domain.ContentHash(o.Output)
		if e != nil {
			return failed(j, "MalformedOutput: "+e.Error())
		}
		records = append(records, workflow.Evidence{ID: id, TaskID: j.TaskID, Type: s.Output, Version: typ.Version, Stage: d.Stage, Tree: resultTree, Role: o.Role, Session: o.Session, Provenance: true, Fields: fields})
	}

	request, requestErr := parseRequest(j, d, records)
	if requestErr == nil {
		delete(j.Failures, "turn-budget-resume:"+d.Stage)
		delete(j.Failures, "schema-retries:"+d.Stage)
		delete(j.PriorRejections, d.Stage)
	}
	if requestErr != nil {
		// Only the request is malformed: a live change turn's edits already
		// passed the bridge's and the executor's change checks. They are
		// recorded as the task's tree (FAE-VCS-003: carried as the task's own
		// work), so the retry continues from them and the workspace matches
		// the checkpoint; left unrecorded, they would escalate
		// StaleBranchResidue at the next dispatch, and restart and Resume
		// would refuse the changed workspace.
		j.Tree = resultTree
		return failed(j, "MalformedOutput: "+requestErr.Error())
	}
	if operation := j.ReviewStages[d.Stage]; operation != "" && j.Disposition.Live {
		if len(records) != 1 {
			return failed(j, "ReviewGateFailed: missing lifecycle receipt")
		}
		f := records[0].Fields
		if operation != "review_read" {
			resetReviewPoll(&j)
		}
		if operation == "review_create" || operation == "review_revision" || operation == "review_merge" {
			id, _ := f["review_id"].(string)
			if id == "" || (j.ReviewID != "" && id != j.ReviewID) {
				return failed(j, "ReviewGateFailed: changed review identity")
			}
			j.ReviewID = id
		}
		if operation == "review_create" {
			j.ReviewRevision = 1
		}
		if operation == "review_revision" {
			j.ReviewRevision++
			j.ReviewMerged = false
		}
		if operation == "review_merge" && !j.ReviewDisabled {
			if f["review_merged"] != true || f["reconciliation_state"] != "reconciled" {
				return failed(j, "ReviewGateFailed: merge receipt absent")
			}
			j.ReviewMerged = true
			event(&j, Event{Kind: "review-merged", Stage: d.Stage, DispatchID: d.ID, Cause: j.ReviewID, Attempt: d.Attempt}, &events)
		}
	}
	// A recorded stage outcome is a success: its same-reason count restarts.
	repeat(&j, d.Stage, "")
	delete(j.Failures, "turn-budget-resume:"+d.Stage)
	delete(j.TurnCheckpoints, d.Stage)
	for _, a := range records {
		j.Evidence = append(j.Evidence, a)
		p := Provenance{RuntimeMounts: r.RuntimeMounts, ViewConstructionMS: r.ViewConstructionMS, ViewCopiedBytes: r.ViewCopiedBytes, IsolationPolicy: r.IsolationPolicy, Generated: r.Generated, ArtifactID: a.ID, DispatchID: d.ID, TaskID: j.TaskID, Stage: d.Stage, Primitive: s.Primitive, Role: a.Role, Session: a.Session, Tree: resultTree, ProducedAt: r.ProducedAt, BranchID: d.BranchID}
		p.ExecutionKind = j.ExecutionKind
		p.Transcript = transcripts[a.Session]
		p.WorkflowHash = j.WorkflowHash
		p.WorkflowRevision = j.Revision
		if d.ResumeCheckpoint != nil || r.ResumedFromCheckpoint {
			p.ResumedFromCheckpoint = true
			if d.ResumeCheckpoint != nil {
				p.CheckpointSession = d.ResumeCheckpoint.ProviderSession
				p.CheckpointTree = d.ResumeCheckpoint.WorktreeTree
			}
		}
		if r.Checkpoint != nil {
			if p.CheckpointSession == "" {
				p.CheckpointSession = r.Checkpoint.ProviderSession
			}
			if p.CheckpointTree == "" {
				p.CheckpointTree = r.Checkpoint.WorktreeTree
			}
		}
		if role, ok := j.Document.Roles[a.Role]; ok && len(role.Routes) > 0 {
			p.Provider = role.Routes[0].Provider
			p.Model = role.Routes[0].Model
			for _, session := range d.Sessions {
				if session.ID == a.Session && session.Route.Provider != "" {
					p.Provider = session.Route.Provider
					p.Model = session.Route.Model
				}
			}
		}
		for _, in := range d.Inputs {
			p.Inputs = append(p.Inputs, in.ID)
		}
		p.ReadRoots = resolveProvenanceReadRoots(j, d, s, a.Role, a.Session, r, outputs)
		p.Reads = resolveProvenanceReads(j, d, a.Role, a.Session, r, outputs)
		j.Provenance = append(j.Provenance, p)
	}
	recordFollowUps(&j, d, records, &events)
	if s.Primitive == workflow.Merge && (resultTree != d.Tree || (r.NewBase != "" && r.NewBase != d.Base)) {
		event(&j, Event{Kind: "tree-reconciled", Stage: d.Stage, DispatchID: d.ID, Cause: "current-tree tests and reviews required", Attempt: d.Attempt}, &events)
	}
	j.Tree = resultTree
	if r.NewBase != "" {
		j.Base = r.NewBase
		j.ExecutionHead = r.NewBase
	}
	if j.ExecutionKind == "live" && s.Primitive == workflow.VcsCommit && len(records) == 1 {
		if sha, ok := records[0].Fields["sha"].(string); ok && sha != "" {
			j.ExecutionHead = sha
		}
	}
	j.InFlight = nil
	j.CutOffByQuota = false
	j.InterruptedSessionID = ""
	j.QuotaResetAt = time.Time{}
	next, e := route(j, s)
	if j.Disposition.Live && j.ReviewStages[d.Stage] == "review_read" && len(records) == 1 {
		decision, _ := records[0].Fields["review_decision"].(string)
		if decision == "remediation" {
			if j.ReviewRevision >= j.ReviewMaxRevisions || j.ReviewRemediation == "" {
				stop(&j, domain.ReviewGateFailed, "ReviewRevisionCap: remediation unavailable or exhausted", &events)
				return j, events, nil
			}
			commit, _ := records[0].Fields["review_commit"].(string)
			if commit == "" {
				stop(&j, domain.ReviewGateFailed, "remediation commit proof missing", &events)
				return j, events, nil
			}
			j.Base = commit
			j.ExecutionHead = commit
			next = j.ReviewRemediation
			event(&j, Event{Kind: "review-remediation", Stage: d.Stage, DispatchID: d.ID, Cause: j.ReviewID, Attempt: d.Attempt}, &events)
		} else if decision != "ready" {
			parkReviewPoll(&j, d, "review status pending", &events)
			return j, events, nil
		}
		resetReviewPoll(&j)
	}
	if s.Primitive == workflow.Merge && (resultTree != d.Tree || (r.NewBase != "" && r.NewBase != d.Base)) {
		// Conflicted marker trees must enter implementation remediation, not
		// the clean revalidation route. The typed report follows normal routing.
		conflicted := len(records) == 1 && records[0].Fields["result"] == "conflicted"
		// A clean, patch-identical reconcile re-runs only QA (FAE-VCS-005a).
		if !conflicted && !carryReviews(&j, d, resultTree, records, &events) {
			next = s.Reverify
		}
	}
	if routeRequest(&j, d, records, request, &next, &e, &events) {
		return j, events, nil
	}
	if e != nil {
		return input, nil, e
	}
	if failedCheck(&j, d, s, &events) {
		if err := routeFailure(&j, "gate", "recorded stage check failed", d.ID, &events); err != nil {
			return input, nil, err
		}
		return j, events, nil
	}
	if e = move(&j, next, "completed "+d.ID, &events); e != nil {
		return input, nil, e
	}
	return j, events, nil
}

// Typed live outcomes. A stale residue refusal proves nothing launched; a
// rejected change left unrecorded edits; an own-hunk loss is a refused merge.
// Malformed or invalid reports from an unchanged tree retry within the bound.
const (
	ClassStaleResidue     = "stale-residue"
	ClassRejectedChange   = "rejected-change"
	ClassOwnHunkLoss      = "own-hunk-loss"
	ClassMalformedOutput  = "malformed-output"
	ClassValidationFailed = "validation-failed"
	ClassProviderFailover = "provider-failover"
	ClassQuotaCutoff      = "quota-cutoff"
)

// FailoverRotations is the Appendix A per-turn failover rotation bound.
const FailoverRotations = 1

// StallRetries is the Appendix A bound on retries of one stalled turn
// (FAE-RT-005); StallRecoveryKey is its durable counter in Job.Failures.
const (
	StallRetries     = 2
	StallRecoveryKey = "stall-recovery"
)

// AdmissionDeferrals is retained as the historical diagnostic threshold;
// host admission deferrals never consume a failure budget or escalate.
const AdmissionDeferrals = 60

// The durable counters (FAE-ROB-017) of a turn's agent-start attempts
// (`startup-recovery:<stage>:<provider>/<model>`, 3 per binding per turn,
// FAE-RT-004; AgentStartCountKey), its agent starts over every binding
// (`agent-starts:<stage>`, AgentStartAttempts per declared binding per turn,
// kept across a binding's exhaustion, a refusal and a failover), its
// later-session deferrals after earlier review sessions ran
// (`admission-deferred:<stage>`, diagnostic only) and its
// failover rotation (`failover:<stage>`, 1 per turn, FAE-RT-010).
const (
	turnStartupKeyPrefix  = "startup-recovery:"
	turnStartsKeyPrefix   = "agent-starts:"
	turnDeferralKeyPrefix = "admission-deferred:"
	turnFailoverKeyPrefix = "failover:"
)

// clearTurnBounds ends a turn's per-turn recovery bounds: its agent-start
// attempts, per binding and in all, its deferrals before launch, its failover
// rotation and its stall retries (Appendix A). Every end of a turn calls it:
// a completion, a recorded failure (failed), any change of state (enter: an
// escalation, block, restart, resume or remap) and a held dispatch's
// collection (Complete). The next turn, at this stage or another, then gets
// 3 agent-start attempts per binding, 1 failover rotation and 2 stall
// retries.
// These counters bound one turn; they are not per-task budgets, so clearing
// them refunds no attempt, test run, loop count or request (FAE-WF-074).
func clearTurnBounds(j *Job) {
	for key := range j.Failures {
		if key == StallRecoveryKey || strings.HasPrefix(key, turnStartupKeyPrefix) || strings.HasPrefix(key, turnStartsKeyPrefix) || strings.HasPrefix(key, turnDeferralKeyPrefix) || strings.HasPrefix(key, turnFailoverKeyPrefix) {
			delete(j.Failures, key)
		}
	}
}

// bounded is detail as the plane stores it (utf8StoredText), cut to at most
// 1024 bytes on a character boundary. Every caller records the result in a
// stored job or event; a cut inside a multibyte character would read back
// with U+FFFD in its place, so a reason the engine compares after a round
// trip (a refused park decided again each round, FAE-RT-010) would never
// match the one it recomputes.
func bounded(detail string) string {
	detail = utf8StoredText(detail)
	if len(detail) <= 1024 {
		return detail
	}
	cut := 1024
	for cut > 0 && !utf8.RuneStart(detail[cut]) {
		cut--
	}
	return detail[:cut]
}

// utf8StoredText is s as a stored job reads it back: encoding/json, which
// encodes every snapshot, replaces each byte of invalid UTF-8 with U+FFFD.
// Valid text is returned unchanged.
func utf8StoredText(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError)
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// refund returns a never-executed dispatch's charges; Sequence keeps IDs fresh.
func refund(j *Job, d Dispatch) {
	if d.AttemptCharged {
		j.Attempts--
	}
	j.Counts[d.Stage]--
	if d.Config.TestRun {
		j.TestRuns--
	}
	if d.AttemptCharged && j.Attempts > 0 && j.Counts[d.Stage] == 0 {
		j.NewAttempt = true
	}
	if len(d.Notifications) > 0 {
		undelivered := map[string]bool{}
		for _, n := range d.Notifications {
			undelivered[n.ID] = true
		}
		for i := range j.Notifications {
			if undelivered[j.Notifications[i].ID] {
				j.Notifications[i].DeliveredAt = time.Time{}
			}
		}
	}
}

// failed records one bounded stage failure (FAE-ROB-002, FAE-WF-034). The same
// stage failing for the same reason more than the Appendix A repeat bound
// escalates SameGate (Appendix B); a new reason or a success at the stage
// restarts the count, and the task attempt limit bounds failures whose reason
// keeps changing. Review lifecycle receipts keep their kind.
func failed(j Job, reason string) (Job, []Event, error) {
	events := []Event{}
	stage := string(j.State)
	s := j.Document.Stages[stage]
	dispatch := ""
	if j.InFlight != nil {
		dispatch = j.InFlight.ID
	}
	isSchema := strings.HasPrefix(reason, "MalformedOutput:") || strings.HasPrefix(reason, "ValidationFailed:")
	if isSchema && s.Primitive == workflow.AgentTurn && (s.Failure != "" || s.SchemaRetries != nil) {
		if j.PriorRejections == nil {
			j.PriorRejections = map[string][]string{}
		}
		j.PriorRejections[stage] = []string{failureText(reason)}
		key := "schema-retries:" + stage
		j.Failures[key]++
		bound := 1
		if s.SchemaRetries != nil {
			bound = *s.SchemaRetries
		}
		if j.Failures[key] > bound {
			if s.Failure == "" && workflow.BranchFailureJoin(j.Document, stage) == "" {
				stop(&j, domain.SameGate, fmt.Sprintf("%s exhausted schema retries: %s", stage, failureText(reason)), &events)
				return j, events, nil
			}
			if err := routeFailure(&j, "schema", reason, dispatch, &events); err != nil {
				return j, nil, err
			}
			return j, events, nil
		}
		// Review identity and merge receipts are trusted lifecycle floors. A
		// content-configured failure edge cannot turn their refusal into progress.
	} else if (s.Failure != "" || workflow.BranchFailureJoin(j.Document, stage) != "") && !(j.ReviewStages[stage] != "" && strings.HasPrefix(reason, "ReviewGateFailed:")) {
		class := "refused"
		switch {
		case strings.Contains(reason, "TurnBudgetExhausted"):
			class = "budget"
		case s.Primitive == workflow.HostCommand:
			class = "command"
		case strings.Contains(reason, "ReviewGateFailed"):
			class = "gate"
		}
		if err := routeFailure(&j, class, reason, dispatch, &events); err != nil {
			return j, nil, err
		}
		return j, events, nil
	}
	j.InFlight = nil
	// A turn that ends in a recorded failure has ended; the stage's next
	// dispatch is a new turn with fresh agent-start, failover and stall-retry
	// bounds (FAE-RT-004, -005, -010).
	clearTurnBounds(&j)
	j.Reason = reason
	event(&j, Event{Kind: "failure", Stage: stage, Attempt: j.stagedAttempt(), Cause: reason}, &events)
	sum := sha256.Sum256([]byte(reason))
	if n := repeat(&j, stage, hex.EncodeToString(sum[:8])); n > SameGateRepeats {
		kind, detail := domain.SameGate, fmt.Sprintf("%s failed %d times for the same reason: %s", stage, n, reason)
		if rest, ok := strings.CutPrefix(reason, string(domain.ReviewGateFailed)+": "); ok {
			kind, detail = domain.ReviewGateFailed, rest
		}
		stop(&j, kind, detail, &events)
	}
	return j, events, nil
}

// sameGate counts consecutive failures of one gate with an identical reason
// (the evaluated leaves, not evidence IDs). A pass or new reason restarts it.
func sameGate(j *Job, stage string, g workflow.GateResult) int {
	reason := ""
	if !g.Passed {
		raw, _ := json.Marshal(struct {
			Value  workflow.Truth  `json:"value"`
			Leaves []workflow.Leaf `json:"leaves"`
		}{g.Value, g.Leaves})
		sum := sha256.Sum256(raw)
		reason = hex.EncodeToString(sum[:8])
	}
	return repeat(j, stage, reason)
}

// repeat is the durable same-gate counter (FAE-ROB-017): consecutive failures
// of one stage for one reason. An empty reason (a success) or a different
// reason restarts it; it returns the count including this failure.
func repeat(j *Job, stage, reason string) int {
	prefix := "same-gate:" + stage + ":"
	key := ""
	if reason != "" {
		key = prefix + reason
	}
	for k := range j.Failures {
		if strings.HasPrefix(k, prefix) && k != key {
			delete(j.Failures, k)
		}
	}
	if key == "" {
		return 0
	}
	j.Failures[key]++
	return j.Failures[key]
}

// Recover distinguishes an interrupted safe scripted run from unknown execution.
// Only the explicit safeReplay capability permits reissuing the original token.
func Recover(input Job, graceful, safeReplay bool) (Job, *Dispatch, error) {
	if input.Fan != nil && input.Fan.Baseline != nil {
		next, err := RecoverBranchFrames(input, func(view Job) (Job, error) { next, _, err := Recover(view, graceful, safeReplay); return next, err })
		return next, next.InFlight, err
	}
	j := clone(input)
	if e := Validate(j); e != nil {
		return input, nil, e
	}
	if j.InFlight == nil {
		return j, nil, nil
	}
	if _, hasCP := j.CheckpointForStage(j.InFlight.Stage); hasCP || (j.InFlight.ResumeCheckpoint != nil && j.InFlight.ResumeCheckpoint.ProviderSession != "") {
		if j.InFlight.ResumeCheckpoint != nil && j.InFlight.ResumeCheckpoint.ProviderSession != "" {
			j.SaveCheckpoint(j.InFlight.Stage, *j.InFlight.ResumeCheckpoint)
		}
		refund(&j, *j.InFlight)
		j.InFlight = nil
		j.Parked = false
		j.Reason = ""
		return j, nil, nil
	}
	if !safeReplay || j.ExecutionKind == "live" || j.OperatorStage != "" {
		if !graceful && !(j.Parked && j.Reason == "UncertainDispatch") {
			j.Failures["crash-recovery"]++
			events := []Event{}
			event(&j, Event{Kind: "crash-recovery", Stage: j.InFlight.Stage, DispatchID: j.InFlight.ID, Attempt: j.InFlight.Attempt, Cause: "execution outcome unknown; explicit resolution required"}, &events)
		}
		j.Parked = true
		j.Reason = "UncertainDispatch"
		return j, nil, nil
	}
	if !graceful {
		j.Failures["dispatch-recovery"]++
		if j.Failures["dispatch-recovery"] > 2 {
			events := []Event{}
			stop(&j, domain.DispatchRecoveryExhausted, fmt.Sprintf("%s crash recoveries exceeded %d", j.InFlight.Stage, 2), &events)
			return j, nil, nil
		}
	}
	j.Parked = false
	j.Reason = ""
	return j, j.InFlight, nil
}

// ExpectedHead preserves compatibility for jobs admitted before execution-head tracking.
func (j Job) ExpectedHead() string {
	if j.ExecutionHead != "" {
		return j.ExecutionHead
	}
	return j.Base
}

// LatestCommitHead returns the latest broker commit receipt. Validate verifies
// evidence identity and provenance before callers may use this for recovery.
func (j Job) LatestCommitHead() string {
	for i := len(j.Provenance) - 1; i >= 0; i-- {
		p := j.Provenance[i]
		if p.Primitive != workflow.VcsCommit {
			continue
		}
		for _, a := range j.Evidence {
			if a.ID == p.ArtifactID && a.Session == p.Session {
				sha, _ := a.Fields["sha"].(string)
				return sha
			}
		}
		return ""
	}
	return ""
}

func resolveProvenanceReadRoots(j Job, d Dispatch, s workflow.Stage, roleName, sessionID string, r Result, outputs []ReviewResult) map[string]string {
	for _, o := range outputs {
		if o.Session == sessionID && len(o.ReadRoots) > 0 {
			out := make(map[string]string, len(o.ReadRoots))
			for k, v := range o.ReadRoots {
				out[k] = v
			}
			return out
		}
	}
	if len(r.ReadRoots) > 0 {
		out := make(map[string]string, len(r.ReadRoots))
		for k, v := range r.ReadRoots {
			out[k] = v
		}
		return out
	}
	if len(j.ReadRoots) == 0 {
		return nil
	}
	var names []string
	if roleName != "" {
		if role, ok := workflow.EffectiveRole(j.Document, d.TaskClass, roleName); ok {
			names = append(names, role.ReadRoots...)
		} else if role, ok := j.Document.Roles[roleName]; ok {
			names = append(names, role.ReadRoots...)
		}
	}
	if s.Primitive == workflow.HostCommand {
		names = append(names, workflow.StageReadRoots(j.Document, d.TaskClass, s)...)
	}
	if len(names) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, n := range names {
		if id, ok := j.ReadRoots[n]; ok && id != "" {
			out[n] = id
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func resolveProvenanceReads(j Job, d Dispatch, roleName, sessionID string, r Result, outputs []ReviewResult) *workflow.ReadPolicy {
	for _, o := range outputs {
		if o.Session == sessionID && o.Reads != nil {
			return o.Reads
		}
	}
	if r.Reads != nil {
		return r.Reads
	}
	return workflow.EffectiveRoleStageReadPolicy(j.Document, d.TaskClass, roleName, d.Stage, j.Reads, j.DenyPaths)
}

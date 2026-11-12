package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Client is the typed view of the daemon's read and write RPCs that the TUI
// uses (FAE-API-001 reads; the live task-editing writes).
type Client struct {
	c Caller
	// noTaskGetUntil remembers a daemon without TaskGet for a while, so
	// polling does not repeat a refused call (each one is logged).
	mu             sync.Mutex
	noTaskGetUntil time.Time
	now            func() time.Time
}

// New wraps a Caller.
func New(c Caller) *Client { return &Client{c: c, now: time.Now} }

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	raw, err := c.c.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: undecodable result: %w", method, err)
	}
	return nil
}

// maxPages bounds every paged read: 20 pages of 500 rows (10,000 tasks) or
// 100 molecules.
const maxPages = 20

// ParentRef links a child task to the parent visit that started it.
type ParentRef struct {
	Task  string `json:"task"`
	Stage string `json:"stage"`
	Visit int    `json:"visit"`
}

// ChildRef is the child task a parent's SubWorkflow visit waits for, pinned
// to the child workflow id and revision (its own molecule).
type ChildRef struct {
	Stage      string     `json:"stage"`
	Visit      int        `json:"visit"`
	Task       string     `json:"task"`
	WorkflowID string     `json:"workflow_id"`
	Revision   int        `json:"revision"`
	Children   []ChildRef `json:"children,omitempty"`
}

// WorkflowRef pins a child workflow by id and revision.
type WorkflowRef struct {
	ID       string `json:"id"`
	Revision int    `json:"revision"`
}

// Task is one Board row: a task's state (its stage, or a kernel state),
// priority, molecule, target and disposition fields.
type Task struct {
	ExecutionKind    string `json:"execution_kind"`
	ID               string `json:"id"`
	Version          int64  `json:"version"`
	WorkflowID       string `json:"workflow_id"`
	WorkflowRevision int64  `json:"workflow_revision"`
	TargetID         string `json:"target_id"`
	TargetRevision   int64  `json:"target_revision"`
	State            string `json:"state"`
	Priority         int    `json:"priority"`
	RunID            string `json:"run_id"`
	Mode             string `json:"mode"`
	Reason           string `json:"reason,omitempty"`
	InFlight         bool   `json:"in_flight"`
	EscalationKind   string `json:"escalation_kind,omitempty"`
	MoleculeID       string `json:"molecule_id,omitempty"`
	// PendingDelete is the Board's stored flag for a TaskDelete that has not
	// finished (daemon/TASKS.md); a daemon that predates it omits the field.
	PendingDelete bool       `json:"pending_delete,omitempty"`
	Parent        *ParentRef `json:"parent,omitempty"`
	Child         *ChildRef  `json:"child,omitempty"`
}

// Board is every Board page read in one pass.
type Board struct {
	Tasks       []Task
	SnapshotAt  time.Time
	SnapshotAge time.Duration
	// Truncated is set when the page bound stopped the read.
	Truncated bool
}

type boardPage struct {
	Items             []Task    `json:"items"`
	NextCursor        string    `json:"next_cursor"`
	SnapshotAt        time.Time `json:"snapshot_at"`
	SnapshotAgeMillis int64     `json:"snapshot_age_ms"`
}

// Board pages the cached Board read (FAE-API-001), oldest snapshot age kept.
func (c *Client) Board(ctx context.Context, includeRetired bool) (Board, error) {
	var out Board
	after := ""
	for page := 0; ; page++ {
		if page == maxPages {
			out.Truncated = true
			return out, nil
		}
		var p boardPage
		if err := c.call(ctx, "Board", map[string]any{"include_retired": includeRetired, "after": after, "limit": 500}, &p); err != nil {
			return Board{}, err
		}
		if page == 0 || p.SnapshotAt.Before(out.SnapshotAt) {
			out.SnapshotAt = p.SnapshotAt
		}
		if age := time.Duration(p.SnapshotAgeMillis) * time.Millisecond; age > out.SnapshotAge {
			out.SnapshotAge = age
		}
		out.Tasks = append(out.Tasks, p.Items...)
		if p.NextCursor == "" || p.NextCursor == after {
			return out, nil
		}
		after = p.NextCursor
	}
}

// Edge is a backlog dependency edge: blocking edges point from prerequisite
// to dependent; relates edges never gate execution.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// BacklogItem is a backlog row (BacklogShow items).
type BacklogItem struct {
	ID               string    `json:"id"`
	SourceRef        string    `json:"source_ref"`
	Priority         int       `json:"priority"`
	AcceptanceDigest string    `json:"acceptance_digest"`
	WriteClass       string    `json:"write_class"`
	State            string    `json:"state"`
	Admitted         time.Time `json:"admitted_at"`
}

// DeadEdge is a blocking edge whose prerequisite can no longer release it.
type DeadEdge struct {
	Edge *Edge `json:"edge,omitempty"`
	// Dependent alone names a dependent whose retired prerequisite is hidden.
	Dependent string `json:"dependent,omitempty"`
	Outcome   string `json:"prerequisite_outcome"`
}

// Backlog is the BacklogGraph read (BacklogShow): items, edges, the ready
// set, withheld prerequisites, dead edges and deferrals.
type Backlog struct {
	Items     []BacklogItem       `json:"items"`
	Edges     []Edge              `json:"edges"`
	Ready     []string            `json:"ready"`
	Withheld  map[string][]string `json:"withheld"`
	DeadEdges []DeadEdge          `json:"dead_edges"`
	Deferrals map[string]string   `json:"deferrals"`
}

// Backlog reads the backlog graph.
func (c *Client) Backlog(ctx context.Context, includeRetired bool) (Backlog, error) {
	var out Backlog
	err := c.call(ctx, "BacklogShow", map[string]any{"include_retired": includeRetired}, &out)
	return out, err
}

// Stage is one stage of a molecule's graph with the non-terminal tasks at it.
type Stage struct {
	Stage      string       `json:"stage"`
	Primitive  string       `json:"primitive,omitempty"`
	Roles      []string     `json:"roles,omitempty"`
	Workflow   *WorkflowRef `json:"workflow,omitempty"`
	NormalPath bool         `json:"normal_path"`
	Tasks      []string     `json:"tasks"`
}

// Molecule is one running molecule: a workflow revision's run with its
// stage graph in normal-case order and its tasks at each stage.
type Molecule struct {
	ID            string    `json:"molecule_id"`
	WorkflowID    string    `json:"workflow_id"`
	Revision      int64     `json:"revision"`
	ContentDigest string    `json:"content_digest"`
	CreatedAt     time.Time `json:"created_at"`
	RoleOrder     []string  `json:"role_order"`
	Stages        []Stage   `json:"stages"`
	// Error is why the daemon could not resolve the molecule's stage graph
	// (a revision this binary cannot run, FAE-WF-093); its tasks are then
	// listed by state alone.
	Error string `json:"error,omitempty"`
}

// Topology is every page of the Molecules read.
type Topology struct {
	Molecules   []Molecule
	SnapshotAge time.Duration
	Truncated   bool
}

// Topology pages the Molecules read (RPC Topology).
func (c *Client) Topology(ctx context.Context) (Topology, error) {
	return c.TopologyWithOptions(ctx, false)
}

// TopologyWithOptions pages the Molecules read (RPC Topology), including a
// running molecule's retired tasks when includeRetired is true.
func (c *Client) TopologyWithOptions(ctx context.Context, includeRetired bool) (Topology, error) {
	var out Topology
	after := ""
	for page := 0; ; page++ {
		if page == maxPages {
			out.Truncated = true
			return out, nil
		}
		var p struct {
			Molecules         []Molecule `json:"molecules"`
			NextCursor        string     `json:"next_cursor"`
			SnapshotAgeMillis int64      `json:"snapshot_age_ms"`
		}
		params := map[string]any{"limit": 100}
		if includeRetired {
			params["include_retired"] = true
		}
		if after != "" {
			params["after"] = after
		}
		if err := c.call(ctx, "Topology", params, &p); err != nil {
			return Topology{}, err
		}
		if age := time.Duration(p.SnapshotAgeMillis) * time.Millisecond; age > out.SnapshotAge {
			out.SnapshotAge = age
		}
		out.Molecules = append(out.Molecules, p.Molecules...)
		if p.NextCursor == "" || p.NextCursor == after {
			return out, nil
		}
		after = p.NextCursor
	}
}

// Status is the part of the Status read the TUI shows.
type Status struct {
	Instance       string                      `json:"instance"`
	PID            int                         `json:"pid"`
	Uptime         float64                     `json:"uptime_seconds"`
	Adapter        string                      `json:"adapter"`
	ShuttingDown   bool                        `json:"shutting_down"`
	Snapshot       *Summary                    `json:"snapshot"`
	ProviderPools  []ProviderPoolView          `json:"provider_pools,omitempty"`
	DispatchPauses []DispatchPauseView         `json:"dispatch_pauses,omitempty"`
	Panes          []PaneSessionView           `json:"panes,omitempty"`
	ProviderHealth []ProviderBindingHealthView `json:"provider_health_bindings,omitempty"`
	TicketOutbox   *TicketOutboxView           `json:"ticket_outbox,omitempty"`
}

// Summary is Status's snapshot block.
type Summary struct {
	Drained     bool              `json:"drained"`
	Draining    bool              `json:"draining"`
	Active      int               `json:"active"`
	Counts      map[string]int    `json:"counts"`
	Total       int               `json:"total"`
	AgeMillis   int64             `json:"age_ms"`
	Quarantined map[string]string `json:"quarantined,omitempty"`
	Scheduler   struct {
		// WithheldTasks is the latest drive round's reason per withheld task.
		WithheldTasks map[string]string `json:"withheld_tasks"`
		Withheld      map[string]int    `json:"withheld"`
	} `json:"scheduler"`
}

// Status reads the cached Status (without task rows). Nested or map-shaped
// provider_pools, dispatch_pauses, panes, provider_health and ticket_outbox
// blocks are decoded tolerantly so Status never fails on differing wire shapes.
func (c *Client) Status(ctx context.Context) (Status, error) {
	raw, err := c.c.Call(ctx, "Status", map[string]any{})
	if err != nil {
		return Status{}, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return Status{}, nil
	}
	var base struct {
		Instance     string   `json:"instance"`
		PID          int      `json:"pid"`
		Uptime       float64  `json:"uptime_seconds"`
		Adapter      string   `json:"adapter"`
		ShuttingDown bool     `json:"shutting_down"`
		Snapshot     *Summary `json:"snapshot"`
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		return Status{}, fmt.Errorf("Status: undecodable result: %w", err)
	}
	out := Status{
		Instance:     base.Instance,
		PID:          base.PID,
		Uptime:       base.Uptime,
		Adapter:      base.Adapter,
		ShuttingDown: base.ShuttingDown,
		Snapshot:     base.Snapshot,
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) == nil {
		out.ProviderPools = decodeProviderPools(top["provider_pools"])
		out.DispatchPauses = decodeDispatchPauses(top["dispatch_pauses"])
		out.Panes = decodePanes(top["panes"])
		if ph, ok := top["provider_health_bindings"]; ok && len(ph) > 0 && string(ph) != "null" {
			out.ProviderHealth = decodeProviderHealth(ph)
		} else if ph, ok := top["provider_health"]; ok {
			out.ProviderHealth = decodeProviderHealth(ph)
		}
		out.TicketOutbox = decodeTicketOutbox(top["ticket_outbox"], top["snapshot"])
	}
	return out, nil
}

// Alert is one alert row.
type Alert struct {
	ID             int64           `json:"id"`
	Severity       string          `json:"severity"`
	Source         string          `json:"source"`
	Task           string          `json:"task,omitempty"`
	Detail         json.RawMessage `json:"detail"`
	CreatedAt      time.Time       `json:"created_at"`
	AcknowledgedAt *time.Time      `json:"acknowledged_at,omitempty"`
}

// Alerts reads up to 500 unacknowledged alerts.
func (c *Client) Alerts(ctx context.Context) ([]Alert, error) {
	var p struct {
		Alerts []Alert `json:"alerts"`
	}
	err := c.call(ctx, "Alerts", map[string]any{"unacknowledged": true, "limit": 500}, &p)
	return p.Alerts, err
}

// InboxTask is an open task waiting on the operator.
type InboxTask struct {
	Task       string          `json:"task"`
	State      string          `json:"state"`
	Stage      string          `json:"stage,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Dispatch   string          `json:"dispatch,omitempty"`
	Workflow   string          `json:"workflow"`
	Revision   int64           `json:"revision"`
	BlockedVia string          `json:"remediation_route,omitempty"`
	Decision   json.RawMessage `json:"decision,omitempty"`
}

// InboxQuestion is an unanswered Prophet question or proposal.
type InboxQuestion struct {
	Task      string   `json:"task,omitempty"`
	Reason    string   `json:"reason"`
	Proposals []string `json:"proposals,omitempty"`
}

// Inbox is the operator inbox without message threads.
type Inbox struct {
	Escalated []InboxTask     `json:"escalated"`
	Blocked   []InboxTask     `json:"blocked"`
	Uncertain []InboxTask     `json:"uncertain"`
	Decisions []InboxTask     `json:"decisions"`
	Questions []InboxQuestion `json:"questions"`
}

// Inbox reads the inbox items that need the operator.
func (c *Client) Inbox(ctx context.Context) (Inbox, error) {
	var out Inbox
	err := c.call(ctx, "Inbox", map[string]any{"kinds": []string{"escalated", "blocked", "uncertain", "decisions", "questions"}}, &out)
	return out, err
}

// Transition is one row of a task's transition log (History records).
type Transition struct {
	ID        int64     `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Owner     string    `json:"owner"`
	Attempt   int       `json:"attempt"`
	Note      string    `json:"note"`
	MessageID string    `json:"message_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Message is an operator message attached to a task.
type Message struct {
	ID      string    `json:"id"`
	Author  string    `json:"author"`
	Created time.Time `json:"created_at"`
	Text    string    `json:"text"`
}

// History is a task's relational record history: transitions and the
// operator messages attached to it.
type History struct {
	State       string
	MoleculeID  string
	Transitions []Transition
	Messages    []Message
	Truncated   bool
}

// TaskHistory pages TaskHistory {records: true}.
func (c *Client) TaskHistory(ctx context.Context, id string, includeRetired bool) (History, error) {
	var out History
	var after int64
	for page := 0; ; page++ {
		if page == maxPages {
			out.Truncated = true
			return out, nil
		}
		var p struct {
			Task struct {
				State string `json:"state"`
			} `json:"task"`
			MoleculeID  string       `json:"molecule_id"`
			Transitions []Transition `json:"transitions"`
			Messages    []Message    `json:"messages"`
			NextCursor  int64        `json:"next_cursor"`
		}
		if err := c.call(ctx, "TaskHistory", map[string]any{"id": id, "records": true, "after": after, "limit": 500, "include_retired": includeRetired}, &p); err != nil {
			return History{}, err
		}
		if page == 0 {
			out.State, out.MoleculeID, out.Messages = p.Task.State, p.MoleculeID, p.Messages
		}
		out.Transitions = append(out.Transitions, p.Transitions...)
		if p.NextCursor == 0 || p.NextCursor <= after {
			return out, nil
		}
		after = p.NextCursor
	}
}

// Shown is the Show read of one task: its stage, park and escalation.
type Shown struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Stage      string `json:"stage"`
	Parked     bool   `json:"parked"`
	InFlight   bool   `json:"in_flight"`
	Reason     string `json:"reason,omitempty"`
	Escalation string `json:"escalation,omitempty"`
	Priority   int    `json:"priority"`
	WorkflowID string `json:"workflow_id"`
	Revision   int64  `json:"workflow_revision"`
	TargetID   string `json:"target_id"`
	TargetRev  int64  `json:"target_revision"`
}

// Show reads one task's Show summary.
func (c *Client) Show(ctx context.Context, id string, includeRetired bool) (Shown, error) {
	var p struct {
		Tasks []Shown `json:"tasks"`
	}
	if err := c.call(ctx, "Show", map[string]any{"task": id, "include_retired": includeRetired}, &p); err != nil {
		return Shown{}, err
	}
	if len(p.Tasks) != 1 {
		return Shown{}, errors.New("show returned no task")
	}
	return p.Tasks[0], nil
}

// Leases reads provider concurrency pools from the Leases RPC (FAE-SCH-015, FAE-RT-018).
func (c *Client) Leases(ctx context.Context) ([]ProviderPoolView, error) {
	raw, err := c.c.Call(ctx, "Leases", map[string]any{})
	if err != nil {
		return nil, err
	}
	return decodeProviderPools(raw), nil
}

// ProviderHealth reads provider binding health and quota/backoff status (FAE-RT-018).
func (c *Client) ProviderHealth(ctx context.Context) ([]ProviderBindingHealthView, error) {
	raw, err := c.c.Call(ctx, "ProviderHealth", map[string]any{})
	if err != nil {
		return nil, err
	}
	return decodeProviderHealth(raw), nil
}

// DispatchPauses reads active dispatch pauses via DispatchPauseList or Status fallback (FAE-RT-018).
func (c *Client) DispatchPauses(ctx context.Context) ([]DispatchPauseView, error) {
	raw, err := c.c.Call(ctx, "DispatchPauseList", map[string]any{})
	if err == nil {
		return decodeDispatchPauses(raw), nil
	}
	if Unsupported(err) {
		st, stErr := c.Status(ctx)
		if stErr == nil {
			return st.DispatchPauses, nil
		}
	}
	return nil, err
}

// Panes reads active execution panes/sessions from the Panes RPC.
func (c *Client) Panes(ctx context.Context) ([]PaneSessionView, error) {
	raw, err := c.c.Call(ctx, "Panes", map[string]any{})
	if err != nil {
		return nil, err
	}
	return decodePanes(raw), nil
}

// AnalyzeSession reads session metrics for taskID from the AnalyzeSession RPC.
func (c *Client) AnalyzeSession(ctx context.Context, taskID string) (SessionAnalysisView, error) {
	raw, err := c.c.Call(ctx, "AnalyzeSession", map[string]any{"task": taskID, "id": taskID})
	if err != nil && isUnknownFieldError(err) {
		raw, err = c.c.Call(ctx, "AnalyzeSession", map[string]any{"task": taskID})
	}
	if err != nil {
		return SessionAnalysisView{}, err
	}
	return decodeSessionAnalysis(raw, taskID)
}

// TargetConfigs reads target configuration policies from TargetConfigList.
func (c *Client) TargetConfigs(ctx context.Context) ([]TargetConfigView, error) {
	raw, err := c.c.Call(ctx, "TargetConfigList", map[string]any{})
	if err != nil {
		return nil, err
	}
	return decodeTargetConfigs(raw), nil
}

// Events reads recent operator/security/sync events from the Events RPC.
func (c *Client) Events(ctx context.Context, limit int) ([]EventView, error) {
	if limit <= 0 {
		limit = 100
	}
	raw, err := c.c.Call(ctx, "Events", map[string]any{"limit": limit})
	if err != nil {
		return nil, err
	}
	return decodeEvents(raw), nil
}

// ProvenanceChain reads the provenance chain for taskID via ProvenanceChain or TaskProvenance.
func (c *Client) ProvenanceChain(ctx context.Context, taskID string) ([]ProvenanceEntryView, error) {
	raw, err := c.c.Call(ctx, "ProvenanceChain", map[string]any{"task": taskID, "id": taskID})
	if err != nil && isUnknownFieldError(err) {
		raw, err = c.c.Call(ctx, "ProvenanceChain", map[string]any{"commit": taskID})
	}
	if err != nil {
		if raw2, err2 := c.c.Call(ctx, "TaskProvenance", map[string]any{"task": taskID, "id": taskID}); err2 == nil {
			raw, err = raw2, nil
		} else if isUnknownFieldError(err2) {
			if raw3, err3 := c.c.Call(ctx, "TaskProvenance", map[string]any{"id": taskID}); err3 == nil {
				raw, err = raw3, nil
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return decodeProvenanceChain(raw), nil
}

// LandStatus reads landing status for taskID from the LandStatus RPC.
func (c *Client) LandStatus(ctx context.Context, taskID string) (LandStatusView, error) {
	raw, err := c.c.Call(ctx, "LandStatus", map[string]any{"task": taskID, "id": taskID})
	if err != nil && isUnknownFieldError(err) {
		raw, err = c.c.Call(ctx, "LandStatus", map[string]any{"id": taskID})
	}
	if err != nil {
		return LandStatusView{}, err
	}
	return decodeLandStatus(raw, taskID)
}

// SymphonyState reads Symphony / Linear tracker state, falling back gracefully
// to Status.TicketOutbox and Board tasks if the daemon does not serve SymphonyState.
func (c *Client) SymphonyState(ctx context.Context) (SymphonyStateView, error) {
	if raw, err := c.c.Call(ctx, "SymphonyState", map[string]any{}); err == nil && len(raw) > 0 && string(raw) != "null" {
		var out SymphonyStateView
		if json.Unmarshal(raw, &out) == nil && (len(out.Running) > 0 || len(out.Retrying) > 0 || len(out.KnownIssues) > 0 || out.Outbox != (TicketOutboxView{}) || out.TotalTokens > 0 || out.PollIntervalMs > 0) {
			return out, nil
		}
	}
	var out SymphonyStateView
	if st, err := c.Status(ctx); err == nil && st.TicketOutbox != nil {
		out.Outbox = *st.TicketOutbox
	}
	board, boardErr := c.Board(ctx, false)
	backlog, _ := c.Backlog(ctx, false)
	sourceRefs := make(map[string]string, len(backlog.Items))
	for _, item := range backlog.Items {
		if item.SourceRef != "" {
			sourceRefs[item.ID] = item.SourceRef
		}
	}
	if boardErr == nil {
		for _, t := range board.Tasks {
			ref := sourceRefs[t.ID]
			if ref == "" && !looksLikeLinearID(t.ID) {
				continue
			}
			ident := ref
			if ident == "" {
				ident = t.ID
			}
			issue := LinearIssueView{
				ID:           t.ID,
				Identifier:   ident,
				Title:        ident,
				State:        t.State,
				Priority:     t.Priority,
				Dispatchable: t.State != "Done" && t.State != "Cancelled" && t.State != "Retired",
				TaskID:       t.ID,
				Stage:        t.State,
			}
			out.KnownIssues = append(out.KnownIssues, issue)
			if t.InFlight {
				out.Running = append(out.Running, issue)
			} else if t.State == "Escalated" || t.State == "Blocked" {
				out.Retrying = append(out.Retrying, issue)
			}
		}
	}
	return out, nil
}

func looksLikeLinearID(s string) bool {
	i := strings.IndexByte(s, '-')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	for _, r := range s[:i] {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	for _, r := range s[i+1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isUnknownFieldError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "unknown field")
}

func decodeProviderPools(raw json.RawMessage) []ProviderPoolView {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var wrapper map[string]json.RawMessage
	if json.Unmarshal(raw, &wrapper) == nil {
		for _, k := range []string{"provider_pools", "pools", "items"} {
			if inner, ok := wrapper[k]; ok && len(inner) > 0 && string(inner) != "null" {
				return decodeProviderPools(inner)
			}
		}
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		out := make([]ProviderPoolView, 0, len(arr))
		for _, elem := range arr {
			if p, ok := decodeOneProviderPool("", elem); ok {
				out = append(out, p)
			}
		}
		return out
	}
	if wrapper != nil {
		keys := make([]string, 0, len(wrapper))
		for k := range wrapper {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out := make([]ProviderPoolView, 0, len(keys))
		for _, k := range keys {
			if p, ok := decodeOneProviderPool(k, wrapper[k]); ok {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func decodeOneProviderPool(key string, raw json.RawMessage) (ProviderPoolView, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return ProviderPoolView{}, false
	}
	type wirePool struct {
		ID                  string          `json:"id"`
		Provider            string          `json:"provider"`
		CredentialIdentity  string          `json:"credential_identity"`
		Capacity            int             `json:"capacity"`
		AdmissionLimit      int             `json:"admission_limit"`
		Holders             json.RawMessage `json:"holders"`
		Waiting             json.RawMessage `json:"waiting"`
		Paused              bool            `json:"paused"`
		PauseActor          string          `json:"pause_actor"`
		PauseReason         string          `json:"pause_reason"`
		QuotaResetAt        time.Time       `json:"quota_reset_at"`
		QuotaExhaustedUntil time.Time       `json:"quota_exhausted_until"`
		CutOffTaskID        string          `json:"cut_off_task_id"`
		CutOffTask          string          `json:"cut_off_task"`
		QuotaReason         string          `json:"quota_reason"`
	}
	type wireNested struct {
		Pool  *wirePool `json:"pool"`
		Quota *struct {
			ResetAt        time.Time `json:"reset_at"`
			QuotaResetAt   time.Time `json:"quota_reset_at"`
			ExhaustedUntil time.Time `json:"exhausted_until"`
			CutOffTaskID   string    `json:"cut_off_task_id"`
			CutOffTask     string    `json:"cut_off_task"`
			Reason         string    `json:"reason"`
		} `json:"quota"`
		Pause *struct {
			Scope  string `json:"scope"`
			Target string `json:"target"`
			Paused *bool  `json:"paused"`
			Actor  string `json:"actor"`
			Reason string `json:"reason"`
		} `json:"pause"`
	}
	parseHolders := func(h json.RawMessage) []string {
		if len(h) == 0 || string(h) == "null" {
			return nil
		}
		var strs []string
		if json.Unmarshal(h, &strs) == nil {
			return strs
		}
		var objs []struct {
			TaskID string `json:"task_id"`
			Task   string `json:"task"`
			ID     string `json:"id"`
		}
		if json.Unmarshal(h, &objs) == nil {
			out := make([]string, 0, len(objs))
			for _, o := range objs {
				if id := firstNonEmpty(o.TaskID, o.Task, o.ID); id != "" {
					out = append(out, id)
				}
			}
			return out
		}
		return nil
	}
	parseWaiting := func(w json.RawMessage) int {
		if len(w) == 0 || string(w) == "null" {
			return 0
		}
		var n int
		if json.Unmarshal(w, &n) == nil {
			return n
		}
		var list []json.RawMessage
		if json.Unmarshal(w, &list) == nil {
			return len(list)
		}
		return 0
	}
	var nested wireNested
	if json.Unmarshal(raw, &nested) == nil && nested.Pool != nil {
		wp := nested.Pool
		out := ProviderPoolView{
			ID:                  firstNonEmpty(wp.ID, key),
			Provider:            wp.Provider,
			CredentialIdentity:  wp.CredentialIdentity,
			Capacity:            wp.Capacity,
			AdmissionLimit:      wp.AdmissionLimit,
			Holders:             parseHolders(wp.Holders),
			Waiting:             parseWaiting(wp.Waiting),
			Paused:              wp.Paused,
			PauseActor:          wp.PauseActor,
			PauseReason:         wp.PauseReason,
			QuotaResetAt:        wp.QuotaResetAt,
			QuotaExhaustedUntil: wp.QuotaExhaustedUntil,
			CutOffTaskID:        firstNonEmpty(wp.CutOffTaskID, wp.CutOffTask),
			QuotaReason:         wp.QuotaReason,
		}
		if nested.Quota != nil {
			if out.QuotaResetAt.IsZero() {
				out.QuotaResetAt = nested.Quota.ResetAt
				if out.QuotaResetAt.IsZero() {
					out.QuotaResetAt = nested.Quota.QuotaResetAt
				}
			}
			if out.QuotaExhaustedUntil.IsZero() {
				out.QuotaExhaustedUntil = nested.Quota.ExhaustedUntil
			}
			out.CutOffTaskID = firstNonEmpty(out.CutOffTaskID, nested.Quota.CutOffTaskID, nested.Quota.CutOffTask)
			out.QuotaReason = firstNonEmpty(out.QuotaReason, nested.Quota.Reason)
		}
		if nested.Pause != nil {
			if nested.Pause.Paused != nil {
				out.Paused = *nested.Pause.Paused
			} else if nested.Pause.Scope != "" || nested.Pause.Actor != "" || nested.Pause.Reason != "" {
				out.Paused = true
			}
			out.PauseActor = firstNonEmpty(out.PauseActor, nested.Pause.Actor)
			out.PauseReason = firstNonEmpty(out.PauseReason, nested.Pause.Reason)
		}
		return out, out.ID != "" || out.Provider != ""
	}
	var wp wirePool
	if json.Unmarshal(raw, &wp) != nil {
		return ProviderPoolView{}, false
	}
	out := ProviderPoolView{
		ID:                  firstNonEmpty(wp.ID, key),
		Provider:            wp.Provider,
		CredentialIdentity:  wp.CredentialIdentity,
		Capacity:            wp.Capacity,
		AdmissionLimit:      wp.AdmissionLimit,
		Holders:             parseHolders(wp.Holders),
		Waiting:             parseWaiting(wp.Waiting),
		Paused:              wp.Paused,
		PauseActor:          wp.PauseActor,
		PauseReason:         wp.PauseReason,
		QuotaResetAt:        wp.QuotaResetAt,
		QuotaExhaustedUntil: wp.QuotaExhaustedUntil,
		CutOffTaskID:        firstNonEmpty(wp.CutOffTaskID, wp.CutOffTask),
		QuotaReason:         wp.QuotaReason,
	}
	return out, out.ID != "" || out.Provider != "" || out.Capacity > 0 || out.AdmissionLimit > 0
}

func decodeProviderHealth(raw json.RawMessage) []ProviderBindingHealthView {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	type wireBinding struct {
		Provider            string    `json:"provider"`
		Model               string    `json:"model"`
		BindingID           string    `json:"binding_id"`
		ID                  string    `json:"id"`
		Score               float64   `json:"score"`
		EffectiveScore      *float64  `json:"effective_score"`
		Eligible            *bool     `json:"eligible"`
		Outcome             string    `json:"outcome"`
		Failures            int       `json:"failures"`
		QuotaResetAt        time.Time `json:"quota_reset_at"`
		QuotaExhaustedUntil time.Time `json:"quota_exhausted_until"`
		CutOffTaskID        string    `json:"cut_off_task_id"`
		CutOffTask          string    `json:"cut_off_task"`
		RateLimitProbes     int       `json:"rate_limit_probes"`
		NextProbeAt         time.Time `json:"next_probe_at"`
	}
	convert := func(key string, w wireBinding) ProviderBindingHealthView {
		eff := w.Score
		if w.EffectiveScore != nil {
			eff = *w.EffectiveScore
		}
		elig := eff > 0
		if w.Eligible != nil {
			elig = *w.Eligible
		}
		return ProviderBindingHealthView{
			Provider:            w.Provider,
			Model:               w.Model,
			BindingID:           firstNonEmpty(w.BindingID, w.ID, key),
			Score:               w.Score,
			EffectiveScore:      eff,
			Eligible:            elig,
			Outcome:             w.Outcome,
			Failures:            w.Failures,
			QuotaResetAt:        w.QuotaResetAt,
			QuotaExhaustedUntil: w.QuotaExhaustedUntil,
			CutOffTaskID:        firstNonEmpty(w.CutOffTaskID, w.CutOffTask),
			RateLimitProbes:     w.RateLimitProbes,
			NextProbeAt:         w.NextProbeAt,
		}
	}
	var list []wireBinding
	if json.Unmarshal(raw, &list) == nil {
		out := make([]ProviderBindingHealthView, 0, len(list))
		for _, w := range list {
			out = append(out, convert("", w))
		}
		return out
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, k := range []string{"bindings", "provider_health_bindings", "provider_health", "ineligible", "items"} {
			if inner, ok := obj[k]; ok && len(inner) > 0 && string(inner) != "null" {
				if decoded := decodeProviderHealth(inner); len(decoded) > 0 {
					return decoded
				}
			}
		}
		var byKey map[string]wireBinding
		if json.Unmarshal(raw, &byKey) == nil && len(byKey) > 0 {
			keys := make([]string, 0, len(byKey))
			for k := range byKey {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			out := make([]ProviderBindingHealthView, 0, len(keys))
			for _, k := range keys {
				w := byKey[k]
				if w.Provider == "" && w.Model == "" && w.Score == 0 && w.Outcome == "" && w.Eligible == nil {
					continue
				}
				out = append(out, convert(k, w))
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

func decodeDispatchPauses(raw json.RawMessage) []DispatchPauseView {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	type wirePause struct {
		Scope     string    `json:"scope"`
		Target    string    `json:"target"`
		Paused    *bool     `json:"paused"`
		Actor     string    `json:"actor"`
		Reason    string    `json:"reason"`
		UpdatedAt time.Time `json:"updated_at"`
		CreatedAt time.Time `json:"created_at"`
	}
	convert := func(w wirePause) DispatchPauseView {
		paused := true
		if w.Paused != nil {
			paused = *w.Paused
		}
		at := w.UpdatedAt
		if at.IsZero() {
			at = w.CreatedAt
		}
		return DispatchPauseView{
			Scope:     w.Scope,
			Target:    w.Target,
			Paused:    paused,
			Actor:     w.Actor,
			Reason:    w.Reason,
			UpdatedAt: at,
		}
	}
	var list []wirePause
	if json.Unmarshal(raw, &list) == nil {
		out := make([]DispatchPauseView, 0, len(list))
		for _, w := range list {
			out = append(out, convert(w))
		}
		return out
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, k := range []string{"pauses", "dispatch_pauses", "items"} {
			if inner, ok := obj[k]; ok {
				return decodeDispatchPauses(inner)
			}
		}
	}
	return nil
}

func decodePanes(raw json.RawMessage) []PaneSessionView {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	type wirePane struct {
		ID        string    `json:"id"`
		PaneID    string    `json:"pane_id"`
		Name      string    `json:"name"`
		TaskID    string    `json:"task_id"`
		Task      string    `json:"task"`
		Role      string    `json:"role"`
		Stage     string    `json:"stage"`
		Provider  string    `json:"provider"`
		Model     string    `json:"model"`
		PID       int       `json:"pid"`
		StartedAt time.Time `json:"started_at"`
		CreatedAt time.Time `json:"created_at"`
	}
	convert := func(w wirePane) PaneSessionView {
		at := w.StartedAt
		if at.IsZero() {
			at = w.CreatedAt
		}
		return PaneSessionView{
			ID:        firstNonEmpty(w.ID, w.PaneID, w.Name),
			TaskID:    firstNonEmpty(w.TaskID, w.Task),
			Role:      w.Role,
			Stage:     w.Stage,
			Provider:  w.Provider,
			Model:     w.Model,
			PID:       w.PID,
			StartedAt: at,
		}
	}
	var list []wirePane
	if json.Unmarshal(raw, &list) == nil {
		out := make([]PaneSessionView, 0, len(list))
		for _, w := range list {
			out = append(out, convert(w))
		}
		return out
	}
	var strs []string
	if json.Unmarshal(raw, &strs) == nil {
		out := make([]PaneSessionView, 0, len(strs))
		for _, s := range strs {
			if s != "" {
				out = append(out, PaneSessionView{ID: s})
			}
		}
		return out
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, k := range []string{"panes", "sessions", "items", "owned"} {
			if inner, ok := obj[k]; ok && len(inner) > 0 && string(inner) != "null" {
				if decoded := decodePanes(inner); len(decoded) > 0 {
					return decoded
				}
			}
		}
	}
	return nil
}

func decodeTicketOutbox(rawOutbox, rawSnapshot json.RawMessage) *TicketOutboxView {
	parse := func(raw json.RawMessage) *TicketOutboxView {
		if len(raw) == 0 || string(raw) == "null" {
			return nil
		}
		var w struct {
			Pending          *int      `json:"pending"`
			Depth            *int      `json:"depth"`
			OldestAgeSeconds float64   `json:"oldest_age_seconds"`
			MaxAttempts      int       `json:"max_attempts"`
			Failures         int       `json:"failures"`
			LastError        string    `json:"last_error"`
			Error            string    `json:"error"`
			CircuitOpenUntil time.Time `json:"circuit_open_until"`
		}
		if json.Unmarshal(raw, &w) != nil {
			return nil
		}
		if w.Pending == nil && w.Depth == nil && w.OldestAgeSeconds == 0 && w.MaxAttempts == 0 && w.Failures == 0 && w.LastError == "" && w.Error == "" && w.CircuitOpenUntil.IsZero() {
			return nil
		}
		pending := 0
		if w.Pending != nil {
			pending = *w.Pending
		} else if w.Depth != nil {
			pending = *w.Depth
		}
		maxAtt := w.MaxAttempts
		if maxAtt == 0 && w.Failures > 0 {
			maxAtt = w.Failures
		}
		return &TicketOutboxView{
			Pending:          pending,
			OldestAgeSeconds: w.OldestAgeSeconds,
			MaxAttempts:      maxAtt,
			LastError:        firstNonEmpty(w.LastError, w.Error),
			CircuitOpenUntil: w.CircuitOpenUntil,
		}
	}
	if out := parse(rawOutbox); out != nil {
		return out
	}
	if len(rawSnapshot) > 0 && string(rawSnapshot) != "null" {
		var snap map[string]json.RawMessage
		if json.Unmarshal(rawSnapshot, &snap) == nil {
			if out := parse(snap["ticket_outbox"]); out != nil {
				return out
			}
		}
	}
	return nil
}

func decodeSessionAnalysis(raw json.RawMessage, fallbackTaskID string) (SessionAnalysisView, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return SessionAnalysisView{TaskID: fallbackTaskID}, nil
	}
	var wire struct {
		TaskID           string         `json:"task_id"`
		Task             string         `json:"task"`
		Turns            int            `json:"turns"`
		WallSeconds      float64        `json:"wall_seconds"`
		Retries          int            `json:"retries"`
		Parks            int            `json:"parks"`
		LeaseWaitSeconds float64        `json:"lease_wait_seconds"`
		TurnsPerRole     map[string]int `json:"turns_per_role"`
		Metrics          []struct {
			Name  string  `json:"name"`
			Stage string  `json:"stage"`
			Role  string  `json:"role"`
			Class string  `json:"class"`
			Value float64 `json:"value"`
			Count int     `json:"count"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return SessionAnalysisView{}, fmt.Errorf("AnalyzeSession: undecodable result: %w", err)
	}
	out := SessionAnalysisView{
		TaskID:           firstNonEmpty(wire.TaskID, wire.Task, fallbackTaskID),
		Turns:            wire.Turns,
		WallSeconds:      wire.WallSeconds,
		Retries:          wire.Retries,
		Parks:            wire.Parks,
		LeaseWaitSeconds: wire.LeaseWaitSeconds,
		TurnsPerRole:     wire.TurnsPerRole,
	}
	if len(wire.Metrics) > 0 {
		var turnSeconds float64
		for _, m := range wire.Metrics {
			switch m.Name {
			case "turns":
				out.Turns += int(m.Value)
				if m.Role != "" {
					if out.TurnsPerRole == nil {
						out.TurnsPerRole = map[string]int{}
					}
					out.TurnsPerRole[m.Role] += int(m.Value)
				}
			case "stage_wall_seconds":
				out.WallSeconds += m.Value
			case "turn_seconds":
				turnSeconds += m.Value
			case "retries":
				out.Retries += int(m.Value)
			case "parks":
				out.Parks += int(m.Value)
			case "lease_wait_seconds":
				out.LeaseWaitSeconds += m.Value
			}
		}
		if out.WallSeconds == 0 && turnSeconds > 0 {
			out.WallSeconds = turnSeconds
		}
	}
	return out, nil
}

func decodeTargetConfigs(raw json.RawMessage) []TargetConfigView {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	type wireTarget struct {
		ID             string          `json:"id"`
		TargetID       string          `json:"target_id"`
		Revision       int64           `json:"revision"`
		ReadRoots      json.RawMessage `json:"read_roots"`
		DenyPaths      []string        `json:"deny_paths"`
		DocumentGlobs  []string        `json:"document_globs"`
		DocumentChecks json.RawMessage `json:"document_checks"`
		CommitName     string          `json:"commit_name"`
		CommitEmail    string          `json:"commit_email"`
		CommitIdentity *struct {
			Name        string `json:"name"`
			Email       string `json:"email"`
			AuthorName  string `json:"author_name"`
			AuthorEmail string `json:"author_email"`
		} `json:"commit_identity"`
		PushMode   string `json:"push_mode"`
		PushRemote string `json:"push_remote"`
		PushBranch string `json:"push_branch"`
		GitPush    *struct {
			Mode   string `json:"mode"`
			Remote string `json:"remote"`
			Branch string `json:"branch"`
		} `json:"git_push"`
		LandingPolicy *struct {
			DocumentGlobs  []string        `json:"document_globs"`
			DocumentChecks json.RawMessage `json:"document_checks"`
		} `json:"landing_policy"`
		Content *json.RawMessage `json:"content"`
	}
	parseReadRoots := func(rr json.RawMessage) map[string]ReadRootView {
		if len(rr) == 0 || string(rr) == "null" {
			return nil
		}
		var byName map[string]ReadRootView
		if json.Unmarshal(rr, &byName) == nil && len(byName) > 0 {
			for k, v := range byName {
				if v.Name == "" {
					v.Name = k
					byName[k] = v
				}
			}
			return byName
		}
		var list []ReadRootView
		if json.Unmarshal(rr, &list) == nil && len(list) > 0 {
			out := make(map[string]ReadRootView, len(list))
			for _, v := range list {
				if v.Name != "" {
					out[v.Name] = v
				}
			}
			return out
		}
		return nil
	}
	parseDocChecks := func(dc json.RawMessage) []string {
		if len(dc) == 0 || string(dc) == "null" {
			return nil
		}
		var strs []string
		if json.Unmarshal(dc, &strs) == nil {
			return strs
		}
		var objs []struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Command []string `json:"command"`
		}
		if json.Unmarshal(dc, &objs) == nil {
			out := make([]string, 0, len(objs))
			for _, o := range objs {
				label := firstNonEmpty(o.ID, o.Name, strings.Join(o.Command, " "))
				if label != "" {
					out = append(out, label)
				}
			}
			return out
		}
		return nil
	}
	var convert func(w wireTarget) TargetConfigView
	convert = func(w wireTarget) TargetConfigView {
		out := TargetConfigView{
			ID:             firstNonEmpty(w.ID, w.TargetID),
			Revision:       w.Revision,
			ReadRoots:      parseReadRoots(w.ReadRoots),
			DenyPaths:      w.DenyPaths,
			DocumentGlobs:  w.DocumentGlobs,
			DocumentChecks: parseDocChecks(w.DocumentChecks),
			CommitName:     w.CommitName,
			CommitEmail:    w.CommitEmail,
			PushMode:       w.PushMode,
			PushRemote:     w.PushRemote,
			PushBranch:     w.PushBranch,
		}
		if w.CommitIdentity != nil {
			out.CommitName = firstNonEmpty(out.CommitName, w.CommitIdentity.Name, w.CommitIdentity.AuthorName)
			out.CommitEmail = firstNonEmpty(out.CommitEmail, w.CommitIdentity.Email, w.CommitIdentity.AuthorEmail)
		}
		if w.GitPush != nil {
			out.PushMode = firstNonEmpty(out.PushMode, w.GitPush.Mode)
			out.PushRemote = firstNonEmpty(out.PushRemote, w.GitPush.Remote)
			out.PushBranch = firstNonEmpty(out.PushBranch, w.GitPush.Branch)
		}
		if w.LandingPolicy != nil {
			if len(out.DocumentGlobs) == 0 {
				out.DocumentGlobs = w.LandingPolicy.DocumentGlobs
			}
			if len(out.DocumentChecks) == 0 {
				out.DocumentChecks = parseDocChecks(w.LandingPolicy.DocumentChecks)
			}
		}
		if w.Content != nil && len(*w.Content) > 0 && string(*w.Content) != "null" {
			var inner wireTarget
			if json.Unmarshal(*w.Content, &inner) == nil {
				merged := convert(inner)
				if len(out.ReadRoots) == 0 {
					out.ReadRoots = merged.ReadRoots
				}
				if len(out.DenyPaths) == 0 {
					out.DenyPaths = merged.DenyPaths
				}
				if len(out.DocumentGlobs) == 0 {
					out.DocumentGlobs = merged.DocumentGlobs
				}
				if len(out.DocumentChecks) == 0 {
					out.DocumentChecks = merged.DocumentChecks
				}
				out.CommitName = firstNonEmpty(out.CommitName, merged.CommitName)
				out.CommitEmail = firstNonEmpty(out.CommitEmail, merged.CommitEmail)
				out.PushMode = firstNonEmpty(out.PushMode, merged.PushMode)
				out.PushRemote = firstNonEmpty(out.PushRemote, merged.PushRemote)
				out.PushBranch = firstNonEmpty(out.PushBranch, merged.PushBranch)
			}
		}
		return out
	}
	var list []wireTarget
	if json.Unmarshal(raw, &list) == nil {
		out := make([]TargetConfigView, 0, len(list))
		for _, w := range list {
			out = append(out, convert(w))
		}
		return out
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, k := range []string{"targets", "target_configs", "items", "configs"} {
			if inner, ok := obj[k]; ok {
				return decodeTargetConfigs(inner)
			}
		}
	}
	return nil
}

func decodeEvents(raw json.RawMessage) []EventView {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	type wireEvent struct {
		ID        int64           `json:"id"`
		Sequence  int64           `json:"sequence"`
		Kind      string          `json:"kind"`
		Type      string          `json:"type"`
		Task      string          `json:"task"`
		TaskID    string          `json:"task_id"`
		Actor     string          `json:"actor"`
		CreatedAt time.Time       `json:"created_at"`
		At        time.Time       `json:"at"`
		Timestamp time.Time       `json:"timestamp"`
		Summary   string          `json:"summary"`
		Message   string          `json:"message"`
		Payload   json.RawMessage `json:"payload"`
		Detail    json.RawMessage `json:"detail"`
	}
	convert := func(w wireEvent) EventView {
		id := w.ID
		if id == 0 {
			id = w.Sequence
		}
		at := w.CreatedAt
		if at.IsZero() {
			at = w.At
		}
		if at.IsZero() {
			at = w.Timestamp
		}
		payload := w.Payload
		if len(payload) == 0 {
			payload = w.Detail
		}
		return EventView{
			ID:      id,
			Kind:    firstNonEmpty(w.Kind, w.Type),
			Task:    firstNonEmpty(w.Task, w.TaskID),
			Actor:   w.Actor,
			At:      at,
			Summary: firstNonEmpty(w.Summary, w.Message),
			Payload: payload,
		}
	}
	var list []wireEvent
	if json.Unmarshal(raw, &list) == nil {
		out := make([]EventView, 0, len(list))
		for _, w := range list {
			out = append(out, convert(w))
		}
		return out
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, k := range []string{"events", "items"} {
			if inner, ok := obj[k]; ok {
				return decodeEvents(inner)
			}
		}
	}
	return nil
}

func decodeProvenanceChain(raw json.RawMessage) []ProvenanceEntryView {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	type wireProv struct {
		DispatchID string            `json:"dispatch_id"`
		Dispatch   string            `json:"dispatch"`
		Stage      string            `json:"stage"`
		StageID    string            `json:"stage_id"`
		Role       string            `json:"role"`
		Outcome    string            `json:"outcome"`
		Kind       string            `json:"kind"`
		CommitSHA  string            `json:"commit_sha"`
		Commit     string            `json:"commit"`
		ReadRoots  map[string]string `json:"read_roots"`
		Reads      *ReadPolicyView   `json:"reads"`
		CreatedAt  time.Time         `json:"created_at"`
		ProducedAt time.Time         `json:"produced_at"`
	}
	convert := func(w wireProv) ProvenanceEntryView {
		at := w.CreatedAt
		if at.IsZero() {
			at = w.ProducedAt
		}
		return ProvenanceEntryView{
			DispatchID: firstNonEmpty(w.DispatchID, w.Dispatch),
			Stage:      firstNonEmpty(w.Stage, w.StageID),
			Role:       w.Role,
			Outcome:    firstNonEmpty(w.Outcome, w.Kind),
			CommitSHA:  firstNonEmpty(w.CommitSHA, w.Commit),
			ReadRoots:  w.ReadRoots,
			Reads:      w.Reads,
			CreatedAt:  at,
		}
	}
	var list []wireProv
	if json.Unmarshal(raw, &list) == nil {
		out := make([]ProvenanceEntryView, 0, len(list))
		for _, w := range list {
			out = append(out, convert(w))
		}
		return out
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, k := range []string{"chain", "provenance", "entries", "hops", "artifacts", "items"} {
			if inner, ok := obj[k]; ok {
				return decodeProvenanceChain(inner)
			}
		}
	}
	return nil
}

func decodeLandStatus(raw json.RawMessage, fallbackTaskID string) (LandStatusView, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return LandStatusView{TaskID: fallbackTaskID}, nil
	}
	var wire struct {
		TaskID     string `json:"task_id"`
		Task       string `json:"task"`
		ID         string `json:"id"`
		Landed     *bool  `json:"landed"`
		State      string `json:"state"`
		TargetHead string `json:"target_head"`
		Target     string `json:"target"`
		TaskCommit string `json:"task_commit"`
		Commit     string `json:"commit"`
		Reason     string `json:"reason"`
		Error      string `json:"error"`
		Results    []struct {
			Task    string `json:"task"`
			Outcome string `json:"outcome"`
			Landed  string `json:"landed"`
			Tip     string `json:"tip"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return LandStatusView{}, fmt.Errorf("LandStatus: undecodable result: %w", err)
	}
	landed := false
	if wire.Landed != nil {
		landed = *wire.Landed
	} else if wire.State == "landed" {
		landed = true
	}
	targetHead := firstNonEmpty(wire.TargetHead, wire.Target)
	taskCommit := firstNonEmpty(wire.TaskCommit, wire.Commit)
	for _, r := range wire.Results {
		if r.Outcome == "landed" || r.Landed != "" {
			landed = true
			taskCommit = firstNonEmpty(taskCommit, r.Landed)
			targetHead = firstNonEmpty(targetHead, r.Tip, r.Landed)
		}
	}
	return LandStatusView{
		TaskID:     firstNonEmpty(wire.TaskID, wire.Task, wire.ID, fallbackTaskID),
		Landed:     landed,
		TargetHead: targetHead,
		TaskCommit: taskCommit,
		Reason:     firstNonEmpty(wire.Reason, wire.Error, wire.State),
	}, nil
}

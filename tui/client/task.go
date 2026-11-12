package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// TaskDetail is everything the task view shows about one task: its admission
// fields, where it is, why it waits, its edges and the trace of stages it has
// been through. Source says which reads produced it.
type TaskDetail struct {
	ID      string
	Version int64
	Title   string
	Content string
	// ContentOmitted is a description too large to send, of ContentBytes.
	ContentOmitted   bool
	ContentBytes     int64
	Acceptance       string
	AcceptanceDigest string
	Priority         int
	SourceRef        string
	WriteClass       string
	DeclaredFiles    []string
	TestFilter       string
	TestShape        string
	Workflow         string
	WorkflowRevision int64
	Target           string
	TargetRevision   int64
	State            string
	Stage            string
	Molecule         string
	InFlight         bool
	Parked           bool
	Reason           string
	EscalationKind   string
	EscalationReason string
	EscalationDetail string
	EscalatedAt      time.Time
	Attempts         int
	AttemptLimit     int
	Deferred         bool
	DeferredUntil    time.Time
	// PendingDelete is the stored TaskDelete that has not finished, nil when
	// there is none.
	PendingDelete *PendingDelete
	Parent        *ParentRef `json:"parent,omitempty"`
	Child         *ChildRef  `json:"child,omitempty"`
	// SupersededFields are the declared files, test filter and test shape a
	// turn in flight was dispatched under when a mid-turn edit has since
	// changed the task's structured fields, nil otherwise.
	SupersededFields *StructuredFields
	// Readiness is the daemon's readiness document, on one line.
	Readiness string
	EdgesIn   []EdgeRef
	EdgesOut  []EdgeRef
	Trace     []TraceEntry
	// TraceOmitted counts the oldest visits left out to fit the read limit;
	// TraceTruncated marks a task past the transition bound.
	TraceOmitted   int
	TraceTruncated bool
	Edits          []Edit
	// EditsTruncated marks a task with more edits than TaskGet returns.
	EditsTruncated bool
	Messages       []Message
	// Source is "TaskGet", or "TaskHistory" when the daemon has no TaskGet.
	Source string
	// Missing names the fields this daemon did not report.
	Missing []string
	// Extended fields decoded from TaskGet (Waves 19-21):
	FollowUpOf      string                        `json:"follow_up_of,omitempty"`
	TurnBudget      *TurnBudgetView               `json:"turn_budget,omitempty"`
	ReadOnlyFiles   []string                      `json:"read_only_files,omitempty"`
	Reads           *ReadPolicyView               `json:"reads,omitempty"`
	Notifications   []NotificationView            `json:"notifications,omitempty"`
	ApprovedScope   []string                      `json:"approved_scope,omitempty"`
	PendingRequest  *PendingRequestView           `json:"pending_request,omitempty"`
	RequestCounts   map[string]int                `json:"request_counts,omitempty"`
	TurnCheckpoints map[string]TurnCheckpointView `json:"turn_checkpoints,omitempty"`
	Fan             *FanView                      `json:"fan,omitempty"`
	DenyPaths       []string                      `json:"deny_paths,omitempty"`
}

// PendingDelete is TaskGet's `pending_delete` object: the delete's reason,
// the successor it named, when it was asked for, and its Status:
// "waiting-for-turn" (the task's turn in flight is still out), "uncertain"
// (the turn in flight is parked UncertainDispatch after a restart and needs
// resolve-uncertain), "retiring" (nothing is in flight; the next drive round
// retires it) or "failed" (the retirement was refused; the task stays
// Escalated). An older daemon reports only the action and retirement text,
// from which the fields are recovered.
type PendingDelete struct {
	Reason            string
	SupersededBy      string
	RequestedAt       time.Time
	Status            string
	UncertainDispatch bool
}

// StructuredFields are a task's declared files, test filter and test shape.
type StructuredFields struct {
	DeclaredFiles []string `json:"declared_files"`
	TestFilter    string   `json:"test_filter"`
	TestShape     string   `json:"test_shape"`
}

// EdgeRef is one edge seen from the task: Other is the task at the far end
// (the prerequisite of an in-edge, the dependent of an out-edge), OtherState
// that task's state, Dead a blocking edge that can no longer release.
type EdgeRef struct {
	Other      string
	Kind       string
	OtherState string
	Dead       bool
	// Satisfied, when reported, says whether an edge in releases the task.
	Satisfied *bool
}

// TraceEntry is one stage visit.
type TraceEntry struct {
	Stage     string
	EnteredAt time.Time
	LeftAt    *time.Time
	Outcome   string
	// To is where the task went when it left.
	To      string
	Attempt int
}

// Edit is one audited change of the task: an edit of its fields (event
// kind task-edited), an edge change, or its delete.
type Edit struct {
	At      time.Time
	Kind    string
	Fields  []string
	Message string
	Actor   string
	Effect  string
	// Detail is the event's payload, for the kinds the fields above do not
	// describe (an edge change, a delete).
	Detail json.RawMessage
}

// TaskGet reads one task through the TaskGet RPC of the live task-editing
// contract (daemon/TASKS.md).
func (c *Client) TaskGet(ctx context.Context, id string, includeRetired bool) (TaskDetail, error) {
	params := map[string]any{"id": id}
	if includeRetired {
		params["include_retired"] = true
	}
	raw, err := c.c.Call(ctx, "TaskGet", params)
	if err != nil {
		return TaskDetail{}, err
	}
	return DecodeTaskGet(raw)
}

// Unsupported reports a refusal that means the daemon has no such method:
// an older daemon refuses an unknown method as an action nobody may perform.
func Unsupported(err error) bool {
	if err == nil || errors.Is(err, ErrUnreachable) {
		return false
	}
	text := err.Error()
	return Code(err) == "unknown-method" || strings.Contains(text, "not permitted to perform action") || strings.Contains(text, "unknown method")
}

// Snapshot is what the TUI already holds when it asks for a task: the Board
// row and the backlog graph fill in a detail read from an older daemon.
type Snapshot struct {
	Tasks   map[string]Task
	Backlog Backlog
}

// Detail reads one task: TaskGet, or, on a daemon without it, Show and
// TaskHistory joined with the Board row and the backlog graph, with the
// trace rebuilt from the transition log.
func (c *Client) Detail(ctx context.Context, id string, have Snapshot, includeRetired bool) (TaskDetail, error) {
	c.mu.Lock()
	legacy := c.now().Before(c.noTaskGetUntil)
	c.mu.Unlock()
	if legacy {
		return c.legacyDetail(ctx, id, have, includeRetired)
	}
	d, err := c.TaskGet(ctx, id, includeRetired)
	if err == nil {
		if d.ID == "" {
			d.ID = id
		}
		return d, nil
	}
	if !Unsupported(err) {
		return TaskDetail{}, err
	}
	// Ask again in a minute: the daemon may be upgraded and restarted.
	c.mu.Lock()
	c.noTaskGetUntil = c.now().Add(taskGetRetry)
	c.mu.Unlock()
	return c.legacyDetail(ctx, id, have, includeRetired)
}

// taskGetRetry is how long a daemon without TaskGet is read the old way.
const taskGetRetry = time.Minute

func (c *Client) legacyDetail(ctx context.Context, id string, have Snapshot, includeRetired bool) (TaskDetail, error) {
	d := TaskDetail{ID: id, Source: "TaskHistory", Missing: []string{"title", "description", "acceptance", "structured fields", "edits"}}
	if t, ok := have.Tasks[id]; ok {
		d.State, d.Priority, d.Workflow, d.WorkflowRevision = t.State, t.Priority, t.WorkflowID, t.WorkflowRevision
		d.Target, d.TargetRevision, d.Molecule, d.InFlight = t.TargetID, t.TargetRevision, t.MoleculeID, t.InFlight
		d.Reason, d.EscalationKind = t.Reason, t.EscalationKind
		d.Parent, d.Child = t.Parent, t.Child
	}
	shown, err := c.Show(ctx, id, includeRetired)
	if err != nil {
		return TaskDetail{}, err
	}
	d.State, d.Stage, d.Parked, d.InFlight, d.Reason = shown.State, shown.Stage, shown.Parked, shown.InFlight, shown.Reason
	d.Priority, d.Workflow, d.WorkflowRevision, d.Target, d.TargetRevision = shown.Priority, shown.WorkflowID, shown.Revision, shown.TargetID, shown.TargetRev
	if shown.Escalation != "" {
		d.EscalationReason = shown.Escalation
	}
	for _, item := range have.Backlog.Items {
		if item.ID == id {
			d.SourceRef, d.WriteClass, d.AcceptanceDigest = item.SourceRef, item.WriteClass, item.AcceptanceDigest
		}
	}
	d.EdgesIn, d.EdgesOut = EdgesOf(id, have.Backlog, have.Tasks)
	history, err := c.TaskHistory(ctx, id, includeRetired)
	if err != nil {
		return TaskDetail{}, err
	}
	if d.Molecule == "" {
		d.Molecule = history.MoleculeID
	}
	d.Trace, d.Messages = TraceFromTransitions(history.Transitions), history.Messages
	return d, nil
}

// EdgesOf lists a task's in- and out-edges from the backlog graph, with the
// far end's state from the Board and dead edges marked.
func EdgesOf(id string, g Backlog, tasks map[string]Task) (in, out []EdgeRef) {
	dead := map[Edge]bool{}
	for _, d := range g.DeadEdges {
		if d.Edge != nil {
			dead[*d.Edge] = true
		}
	}
	state := func(other string) string {
		if t, ok := tasks[other]; ok {
			return t.State
		}
		return "unknown"
	}
	for _, e := range g.Edges {
		switch id {
		case e.To:
			in = append(in, EdgeRef{Other: e.From, Kind: e.Kind, OtherState: state(e.From), Dead: dead[e]})
		case e.From:
			out = append(out, EdgeRef{Other: e.To, Kind: e.Kind, OtherState: state(e.To), Dead: dead[e]})
		}
	}
	return in, out
}

// TraceFromTransitions rebuilds the stage trace from a transition log: each
// transition enters its target stage and leaves the previous one. The first
// transition's source stage (normally Pending) opens the trace.
func TraceFromTransitions(ts []Transition) []TraceEntry {
	out := []TraceEntry{}
	for i, t := range ts {
		if i == 0 && t.From != "" {
			out = append(out, TraceEntry{Stage: t.From, Attempt: t.Attempt})
		}
		if len(out) > 0 {
			left := t.CreatedAt
			prev := &out[len(out)-1]
			prev.LeftAt, prev.Outcome, prev.To = &left, strings.TrimSpace(t.Note), t.To
		}
		out = append(out, TraceEntry{Stage: t.To, EnteredAt: t.CreatedAt, Attempt: t.Attempt})
	}
	return out
}

// DecodeTaskGet decodes a TaskGet result. It takes the contract's field names
// and tolerates the near spellings an extension may use (nested admission or
// escalation objects, edges as one list or split in and out), so a richer
// daemon payload still decodes.
func DecodeTaskGet(raw json.RawMessage) (TaskDetail, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return TaskDetail{}, fmt.Errorf("TaskGet: undecodable result: %w", err)
	}
	if top == nil {
		return TaskDetail{}, errors.New("TaskGet: empty result")
	}
	// Admission and job fields may sit at the top level or in a nested object.
	scopes := []map[string]json.RawMessage{top}
	for _, name := range []string{"admission", "task", "fields", "job"} {
		var nested map[string]json.RawMessage
		if v, ok := top[name]; ok && json.Unmarshal(v, &nested) == nil && nested != nil {
			scopes = append(scopes, nested)
		}
	}
	rawField := func(keys ...string) (json.RawMessage, bool) {
		for _, scope := range scopes {
			for _, k := range keys {
				if v, ok := scope[k]; ok && len(v) > 0 && string(v) != "null" {
					return v, true
				}
			}
		}
		return nil, false
	}
	str := func(keys ...string) string {
		for _, scope := range scopes {
			for _, k := range keys {
				var s string
				if v, ok := scope[k]; ok && json.Unmarshal(v, &s) == nil && s != "" {
					return s
				}
			}
		}
		return ""
	}
	num := func(keys ...string) int64 {
		for _, scope := range scopes {
			for _, k := range keys {
				var n int64
				if v, ok := scope[k]; ok && json.Unmarshal(v, &n) == nil && n != 0 {
					return n
				}
			}
		}
		return 0
	}
	flag := func(keys ...string) bool {
		for _, scope := range scopes {
			for _, k := range keys {
				var b bool
				if v, ok := scope[k]; ok && json.Unmarshal(v, &b) == nil && b {
					return true
				}
			}
		}
		return false
	}
	list := func(keys ...string) []string {
		for _, scope := range scopes {
			for _, k := range keys {
				var l []string
				if v, ok := scope[k]; ok && json.Unmarshal(v, &l) == nil && len(l) > 0 {
					return l
				}
			}
		}
		return nil
	}
	d := TaskDetail{Source: "TaskGet"}
	d.ID = str("id", "task_id")
	d.Title = str("title")
	d.Content = str("content", "description")
	d.Acceptance = str("acceptance")
	d.AcceptanceDigest = str("acceptance_digest", "content_sha256")
	d.Priority = int(num("priority"))
	d.SourceRef = str("source_ref", "source")
	d.WriteClass = str("write_class")
	d.DeclaredFiles = list("declared_files")
	d.TestFilter = str("test_filter")
	d.TestShape = str("test_shape")
	d.Workflow = str("workflow_id", "workflow")
	d.WorkflowRevision = num("workflow_revision", "revision")
	d.Target = str("target_id", "target_config", "target")
	d.TargetRevision = num("target_revision")
	d.State = str("state")
	d.Stage = str("stage", "current_stage")
	d.Molecule = str("molecule_id", "molecule")
	d.InFlight = flag("in_flight")
	d.Parked = flag("parked")
	d.Reason = str("reason", "park_reason")
	d.EscalationKind = str("escalation_kind")
	d.EscalationReason = str("escalation_reason", "escalation_detail")
	if detail := str("escalation_detail"); detail != d.EscalationReason {
		d.EscalationDetail = detail
	}
	d.TraceOmitted, d.TraceTruncated = int(num("trace_omitted")), flag("trace_truncated")
	d.Version = num("version")
	d.Attempts, d.AttemptLimit = int(num("attempts")), int(num("attempt_limit"))
	d.ContentOmitted, d.ContentBytes = flag("content_omitted"), num("content_bytes")
	d.Deferred = flag("deferred")
	for _, k := range []struct {
		key string
		to  *time.Time
	}{{"escalated_at", &d.EscalatedAt}, {"deferred_until", &d.DeferredUntil}} {
		if v, ok := top[k.key]; ok {
			_ = json.Unmarshal(v, k.to)
		}
	}
	d.EditsTruncated = flag("edits_truncated")
	if v, ok := top["pending_delete"]; ok {
		d.PendingDelete = decodePendingDelete(v)
	}
	if v, ok := top["parent"]; ok && string(v) != "null" {
		var p ParentRef
		if json.Unmarshal(v, &p) == nil && p.Task != "" {
			d.Parent = &p
		}
	}
	if v, ok := top["child"]; ok && string(v) != "null" {
		var c ChildRef
		if json.Unmarshal(v, &c) == nil && c.Task != "" {
			d.Child = &c
		}
	}
	if v, ok := top["superseded_fields"]; ok {
		d.SupersededFields = decodeSupersededFields(v)
	}
	if v, ok := top["readiness"]; ok && string(v) != "null" {
		d.Readiness = readinessText(v)
	}
	// A workflow or target given as an object {id, revision}.
	for _, k := range []string{"workflow", "target"} {
		var ref struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
		}
		if v, ok := top[k]; ok && json.Unmarshal(v, &ref) == nil && ref.ID != "" {
			if k == "workflow" {
				d.Workflow, d.WorkflowRevision = ref.ID, ref.Revision
			} else {
				d.Target, d.TargetRevision = ref.ID, ref.Revision
			}
		}
	}
	var esc struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
	if v, ok := top["escalation"]; ok && json.Unmarshal(v, &esc) == nil {
		d.EscalationKind = firstNonEmpty(d.EscalationKind, esc.Kind)
		d.EscalationReason = firstNonEmpty(d.EscalationReason, esc.Reason, esc.Detail)
	}
	if d.PendingDelete != nil && (d.Reason == "UncertainDispatch" || d.EscalationReason == "UncertainDispatch" || d.EscalationKind == "UncertainDispatch") {
		d.PendingDelete.UncertainDispatch = true
	}
	d.EdgesIn, d.EdgesOut = decodeEdges(top, d.ID)
	d.Trace, d.Edits = decodeTrace(top)
	if v, ok := top["messages"]; ok {
		_ = json.Unmarshal(v, &d.Messages)
	}
	d.FollowUpOf = str("follow_up_of", "predecessor_task")
	if v, ok := rawField("turn_budget", "budget"); ok {
		var tb TurnBudgetView
		if json.Unmarshal(v, &tb) == nil && (tb != TurnBudgetView{}) {
			d.TurnBudget = &tb
		}
	}
	d.ReadOnlyFiles = list("read_only_files")
	if v, ok := rawField("reads"); ok {
		var rp ReadPolicyView
		if json.Unmarshal(v, &rp) == nil && (len(rp.Allow) > 0 || len(rp.Deny) > 0) {
			d.Reads = &rp
		}
	}
	if v, ok := rawField("notifications"); ok {
		d.Notifications = decodeNotifications(v)
	}
	d.ApprovedScope = list("approved_scope")
	if v, ok := rawField("pending_request"); ok {
		var pr PendingRequestView
		if json.Unmarshal(v, &pr) == nil && (pr.Kind != "" || len(pr.Paths) > 0 || len(pr.Files) > 0 || pr.TestFile != "" || pr.Reason != "") {
			d.PendingRequest = &pr
		}
	}
	if v, ok := rawField("request_counts"); ok {
		var rc map[string]int
		if json.Unmarshal(v, &rc) == nil && len(rc) > 0 {
			d.RequestCounts = rc
		}
	}
	if v, ok := rawField("turn_checkpoints"); ok {
		d.TurnCheckpoints = decodeTurnCheckpoints(v)
	}
	if v, ok := rawField("fan", "active_fan"); ok {
		d.Fan = decodeFan(v)
	}
	d.DenyPaths = list("deny_paths")
	return d, nil
}

func decodeNotifications(raw json.RawMessage) []NotificationView {
	type wireNotif struct {
		ID          string    `json:"id"`
		Kind        string    `json:"kind"`
		Delivery    string    `json:"delivery"`
		Summary     string    `json:"summary"`
		Body        string    `json:"body"`
		FromTask    string    `json:"from_task"`
		FromTaskID  string    `json:"from_task_id"`
		Actor       string    `json:"actor"`
		RelayedBy   string    `json:"relayed_by"`
		CreatedAt   time.Time `json:"created_at"`
		DeliveredAt time.Time `json:"delivered_at"`
	}
	var items []wireNotif
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return nil
	}
	out := make([]NotificationView, 0, len(items))
	for _, w := range items {
		out = append(out, NotificationView{
			ID:          w.ID,
			Kind:        w.Kind,
			Delivery:    w.Delivery,
			Summary:     firstNonEmpty(w.Summary, w.Body),
			FromTask:    firstNonEmpty(w.FromTask, w.FromTaskID),
			Actor:       firstNonEmpty(w.Actor, w.RelayedBy),
			CreatedAt:   w.CreatedAt,
			DeliveredAt: w.DeliveredAt,
		})
	}
	return out
}

func decodeTurnCheckpoints(raw json.RawMessage) map[string]TurnCheckpointView {
	type wireCP struct {
		ProviderSession string    `json:"provider_session"`
		WorktreeTree    string    `json:"worktree_tree"`
		Stage           string    `json:"stage"`
		StageID         string    `json:"stage_id"`
		AssistantTurns  int       `json:"assistant_turns"`
		WallSeconds     int       `json:"wall_seconds"`
		UpdatedAt       time.Time `json:"updated_at"`
	}
	var byKey map[string]wireCP
	if json.Unmarshal(raw, &byKey) == nil && len(byKey) > 0 {
		out := make(map[string]TurnCheckpointView, len(byKey))
		for k, w := range byKey {
			out[k] = TurnCheckpointView{
				ProviderSession: w.ProviderSession,
				WorktreeTree:    w.WorktreeTree,
				Stage:           firstNonEmpty(w.Stage, w.StageID, k),
				AssistantTurns:  w.AssistantTurns,
				WallSeconds:     w.WallSeconds,
				UpdatedAt:       w.UpdatedAt,
			}
		}
		return out
	}
	var list []wireCP
	if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
		out := make(map[string]TurnCheckpointView, len(list))
		for _, w := range list {
			stage := firstNonEmpty(w.Stage, w.StageID)
			if stage == "" {
				continue
			}
			out[stage] = TurnCheckpointView{
				ProviderSession: w.ProviderSession,
				WorktreeTree:    w.WorktreeTree,
				Stage:           stage,
				AssistantTurns:  w.AssistantTurns,
				WallSeconds:     w.WallSeconds,
				UpdatedAt:       w.UpdatedAt,
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func decodeFan(raw json.RawMessage) *FanView {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	type wireBranch struct {
		ID           string          `json:"id"`
		Status       string          `json:"status"`
		Stage        string          `json:"stage"`
		StageID      string          `json:"stage_id"`
		InFlight     json.RawMessage `json:"in_flight"`
		WriteScope   []string        `json:"write_scope"`
		CommitSHA    string          `json:"commit_sha"`
		BranchRef    string          `json:"branch_ref"`
		Tree         string          `json:"tree"`
		WorktreeTree string          `json:"worktree_tree"`
		BaseTree     string          `json:"base_tree"`
		Item         json.RawMessage `json:"item"`
	}
	type wireFan struct {
		Stage      string       `json:"stage"`
		StageID    string       `json:"stage_id"`
		Join       string       `json:"join"`
		JoinPolicy string       `json:"join_policy"`
		Parallel   bool         `json:"parallel"`
		Branches   []wireBranch `json:"branches"`
	}
	var wf wireFan
	if json.Unmarshal(raw, &wf) != nil {
		return nil
	}
	stage := firstNonEmpty(wf.Stage, wf.StageID)
	join := firstNonEmpty(wf.Join, wf.JoinPolicy)
	if stage == "" && join == "" && !wf.Parallel && len(wf.Branches) == 0 {
		return nil
	}
	fv := &FanView{
		Stage:    stage,
		Join:     join,
		Parallel: wf.Parallel,
	}
	for _, b := range wf.Branches {
		inFlight := false
		if len(b.InFlight) > 0 && string(b.InFlight) != "null" {
			var bFlag bool
			if json.Unmarshal(b.InFlight, &bFlag) == nil {
				inFlight = bFlag
			} else {
				var obj map[string]any
				if json.Unmarshal(b.InFlight, &obj) == nil && len(obj) > 0 {
					inFlight = true
				}
			}
		}
		var itemStr string
		if len(b.Item) > 0 && string(b.Item) != "null" {
			if json.Unmarshal(b.Item, &itemStr) != nil {
				itemStr = strings.TrimSpace(string(b.Item))
			}
		}
		fv.Branches = append(fv.Branches, FanBranchView{
			ID:         b.ID,
			Status:     b.Status,
			Stage:      firstNonEmpty(b.Stage, b.StageID),
			InFlight:   inFlight,
			WriteScope: b.WriteScope,
			CommitSHA:  b.CommitSHA,
			BranchRef:  b.BranchRef,
			Tree:       firstNonEmpty(b.Tree, b.WorktreeTree),
			BaseTree:   b.BaseTree,
			Item:       itemStr,
		})
	}
	return fv
}

// decodePendingDelete reads the `pending_delete` object: the contract's
// reason, superseded_by, requested_at and status, or, from a daemon that
// stores only the retirement text ("obsolete REASON" or "superseded-by ID")
// and the dispatch it waits for, the same fields recovered from those. A
// null or empty object is no pending delete.
func decodePendingDelete(raw json.RawMessage) *PendingDelete {
	var w struct {
		Reason            string    `json:"reason"`
		SupersededBy      string    `json:"superseded_by"`
		RequestedAt       time.Time `json:"requested_at"`
		Status            string    `json:"status"`
		Action            string    `json:"action"`
		Retirement        string    `json:"retirement"`
		Dispatch          string    `json:"dispatch"`
		UncertainDispatch bool      `json:"uncertain_dispatch"`
		Uncertain         bool      `json:"uncertain"`
	}
	if string(raw) == "null" || json.Unmarshal(raw, &w) != nil {
		return nil
	}
	if w.Reason == "" && w.SupersededBy == "" && w.Status == "" && w.Action == "" && w.Retirement == "" && !w.UncertainDispatch && !w.Uncertain {
		return nil
	}
	p := &PendingDelete{Reason: w.Reason, SupersededBy: w.SupersededBy, RequestedAt: w.RequestedAt, Status: w.Status, UncertainDispatch: w.UncertainDispatch || w.Uncertain || w.Status == "uncertain"}
	if p.Reason == "" && p.SupersededBy == "" {
		if successor, ok := strings.CutPrefix(w.Retirement, "superseded-by "); ok {
			p.SupersededBy = strings.TrimSpace(successor)
		} else {
			p.Reason = strings.TrimSpace(strings.TrimPrefix(w.Retirement, "obsolete"))
		}
	}
	if p.Status == "" {
		p.Status = "retiring"
		if w.Dispatch != "" {
			p.Status = "waiting-for-turn"
		}
	}
	return p
}

// decodeSupersededFields reads TaskGet's `superseded_fields` object: the
// structured fields a turn in flight still runs under after an in-flight
// edit. A null or empty object is nil.
func decodeSupersededFields(raw json.RawMessage) *StructuredFields {
	var m map[string]json.RawMessage
	if string(raw) == "null" || json.Unmarshal(raw, &m) != nil || len(m) == 0 {
		return nil
	}
	var s StructuredFields
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return &s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// wireEdge is an edge as TaskGet may report it.
type wireEdge struct {
	From              string `json:"from"`
	To                string `json:"to"`
	Kind              string `json:"kind"`
	Task              string `json:"task"`
	ID                string `json:"id"`
	State             string `json:"state"`
	PrerequisiteState string `json:"prerequisite_state"`
	DependentState    string `json:"dependent_state"`
	FromState         string `json:"from_state"`
	ToState           string `json:"to_state"`
	Dead              bool   `json:"dead"`
	Satisfied         *bool  `json:"satisfied"`
}

// deadOutcomes are the prerequisite outcomes that never release a blocking
// edge unless the retirement released it (BACKLOG.md: dead edges).
var deadOutcomes = []string{"Cancelled", "Retired", "Escalated", "missing"}

func decodeEdges(top map[string]json.RawMessage, id string) (in, out []EdgeRef) {
	convert := func(e wireEdge, incoming bool) EdgeRef {
		r := EdgeRef{Kind: e.Kind, Dead: e.Dead, Satisfied: e.Satisfied}
		if incoming {
			r.Other = firstNonEmpty(e.From, e.Task, e.ID)
			r.OtherState = firstNonEmpty(e.PrerequisiteState, e.FromState, e.State)
		} else {
			r.Other = firstNonEmpty(e.To, e.Task, e.ID)
			r.OtherState = firstNonEmpty(e.DependentState, e.ToState, e.State)
		}
		if r.Kind == "" {
			r.Kind = "blocking"
		}
		if incoming && r.Kind == "blocking" && e.Satisfied != nil && !*e.Satisfied && slices.Contains(deadOutcomes, r.OtherState) {
			r.Dead = true
		}
		return r
	}
	split := func(edges []wireEdge, incoming bool) []EdgeRef {
		refs := []EdgeRef{}
		for _, e := range edges {
			refs = append(refs, convert(e, incoming))
		}
		return refs
	}
	var pair struct {
		In  []wireEdge `json:"in"`
		Out []wireEdge `json:"out"`
	}
	if v, ok := top["edges"]; ok {
		if json.Unmarshal(v, &pair) == nil && (pair.In != nil || pair.Out != nil) {
			return split(pair.In, true), split(pair.Out, false)
		}
		var flat []wireEdge
		if json.Unmarshal(v, &flat) == nil {
			for _, e := range flat {
				if e.To == id {
					in = append(in, convert(e, true))
				} else {
					out = append(out, convert(e, false))
				}
			}
			return in, out
		}
	}
	for _, names := range [][2]string{{"edges_in", "edges_out"}, {"in_edges", "out_edges"}, {"prerequisites", "dependents"}} {
		var a, b []wireEdge
		_, okA := top[names[0]]
		_, okB := top[names[1]]
		if !okA && !okB {
			continue
		}
		_ = json.Unmarshal(top[names[0]], &a)
		_ = json.Unmarshal(top[names[1]], &b)
		return split(a, true), split(b, false)
	}
	return nil, nil
}

// wireTrace is a trace or edit entry as TaskGet may report it.
type wireTrace struct {
	Kind      string     `json:"kind"`
	Stage     string     `json:"stage"`
	EnteredAt time.Time  `json:"entered_at"`
	LeftAt    *time.Time `json:"left_at"`
	Outcome   string     `json:"outcome"`
	To        string     `json:"to"`
	Attempt   int        `json:"attempt"`
	At        time.Time  `json:"at"`
	EditedAt  time.Time  `json:"edited_at"`
	Timestamp time.Time  `json:"timestamp"`
	Fields    []string   `json:"fields"`
	Changed   []string   `json:"changed"`
	Message   string     `json:"authorizing_message"`
	MessageID string     `json:"message_id"`
	Actor     string     `json:"actor"`
	Effect    string     `json:"effect"`
	// Detail is an edit event's payload, which may hold the fields above.
	Detail json.RawMessage `json:"detail"`
	// Edits nested in a stage visit.
	Edits []wireTrace `json:"edits"`
}

func (w wireTrace) edit() Edit {
	if len(w.Detail) > 0 {
		var inner wireTrace
		if json.Unmarshal(w.Detail, &inner) == nil {
			w.Fields, w.Changed = slices.Concat(w.Fields, inner.Fields), slices.Concat(w.Changed, inner.Changed)
			w.Message = firstNonEmpty(w.Message, inner.Message)
			w.MessageID = firstNonEmpty(w.MessageID, inner.MessageID)
			w.Actor, w.Effect = firstNonEmpty(w.Actor, inner.Actor), firstNonEmpty(w.Effect, inner.Effect)
		}
	}
	at := w.At
	for _, t := range []time.Time{w.EditedAt, w.Timestamp, w.EnteredAt} {
		if at.IsZero() {
			at = t
		}
	}
	return Edit{At: at, Kind: w.Kind, Fields: slices.Concat(w.Fields, w.Changed), Message: firstNonEmpty(w.Message, w.MessageID), Actor: w.Actor, Effect: w.Effect, Detail: w.Detail}
}

func decodeTrace(top map[string]json.RawMessage) ([]TraceEntry, []Edit) {
	var entries []wireTrace
	var edits []wireTrace
	if v, ok := top["trace"]; ok {
		if json.Unmarshal(v, &entries) != nil {
			// The trace may be an object holding the stages and the edits.
			var both struct {
				Stages []wireTrace `json:"stages"`
				Edits  []wireTrace `json:"edits"`
			}
			if json.Unmarshal(v, &both) == nil {
				entries, edits = both.Stages, both.Edits
			}
		}
	}
	if v, ok := top["edits"]; ok {
		var more []wireTrace
		if json.Unmarshal(v, &more) == nil {
			edits = append(edits, more...)
		}
	}
	// Edits nested in the stage visits count only when the task lists none
	// of its own: the two lists hold the same events.
	if len(edits) == 0 {
		for _, e := range entries {
			edits = append(edits, e.Edits...)
		}
	}
	trace := []TraceEntry{}
	out := []Edit{}
	for _, e := range entries {
		if e.Stage == "" && (e.Kind != "" || len(e.Fields) > 0 || len(e.Changed) > 0) {
			out = append(out, e.edit())
			continue
		}
		trace = append(trace, TraceEntry{Stage: e.Stage, EnteredAt: e.EnteredAt, LeftAt: e.LeftAt, Outcome: e.Outcome, To: e.To, Attempt: e.Attempt})
	}
	for _, e := range edits {
		out = append(out, e.edit())
	}
	slices.SortStableFunc(out, func(a, b Edit) int { return a.At.Compare(b.At) })
	return trace, out
}

// readinessText describes a readiness document ({"state", "ready",
// "withheld_by"}) in words, or gives it on one line when it has another shape.
func readinessText(raw json.RawMessage) string {
	var r struct {
		State    string   `json:"state"`
		Ready    *bool    `json:"ready"`
		Withheld []string `json:"withheld_by"`
		Deferred any      `json:"deferred"`
	}
	if json.Unmarshal(raw, &r) == nil && r.Ready != nil {
		switch {
		case *r.Ready:
			return "ready"
		case len(r.Withheld) > 0:
			return "withheld by " + strings.Join(r.Withheld, ", ")
		case r.Deferred != nil && r.Deferred != false:
			return "deferred"
		case r.State != "":
			return "not ready (" + r.State + ")"
		}
		return "not ready"
	}
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

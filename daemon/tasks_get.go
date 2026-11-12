package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// TaskGetParams reads one task. A retired (deleted) task is hidden unless
// IncludeRetired, as History and Show hide it.
type TaskGetParams struct {
	ID             string `json:"id"`
	IncludeRetired bool   `json:"include_retired,omitempty"`
}

// TaskDetailView aliases TaskView for callers referencing TaskDetailView (FAE-OPS-034).
type TaskDetailView = TaskView

// TaskView is TaskGet's answer: one task's admission fields, where it is,
// its edges with the other ends' states, and its trace.
type TaskView struct {
	ID         string                 `json:"id"`
	Version    int64                  `json:"version"`
	FollowUpOf string                 `json:"follow_up_of,omitempty"`
	TurnBudget *engine.TurnBudgetSpec `json:"turn_budget,omitempty"`
	// Admission fields (FAE-CLI-014).
	Title         string                    `json:"title"`
	Content       string                    `json:"content"`
	Priority      int                       `json:"priority"`
	SourceRef     string                    `json:"source_ref"`
	WriteClass    string                    `json:"write_class"`
	DeclaredFiles []string                  `json:"declared_files"`
	ReadOnlyFiles []string                  `json:"read_only_files,omitempty"`
	Reads         *workflow.ReadPolicy      `json:"reads,omitempty"`
	Notifications []engine.TaskNotification `json:"notifications,omitempty"`
	TestFilter    string                    `json:"test_filter"`
	TestShape     string                    `json:"test_shape"`
	TaskClass     string                    `json:"task_class,omitempty"`
	// SupersededFields are the declared files, test filter and test shape the
	// turn in flight still runs under when a mid-turn edit has changed the
	// task's structured fields; omitted once the turn finishes.
	SupersededFields *TaskSupersededFields `json:"superseded_fields,omitempty"`
	// ContentOmitted is set when the content alone would not fit the
	// response; ContentBytes and ContentSHA256 then describe it.
	ContentOmitted   bool      `json:"content_omitted,omitempty"`
	ContentBytes     int       `json:"content_bytes"`
	ContentSHA256    string    `json:"content_sha256"`
	WorkflowID       string    `json:"workflow_id"`
	WorkflowRevision int64     `json:"workflow_revision"`
	TargetID         string    `json:"target_id"`
	TargetConfig     string    `json:"target_config,omitempty"`
	TargetRevision   int64     `json:"target_revision,omitempty"`
	TargetHash       string    `json:"target_hash,omitempty"`
	Mode             string    `json:"mode"`
	RunID            string    `json:"run_id"`
	AdmittedAt       time.Time `json:"admitted_at"`
	// Where it is.
	State         string     `json:"state"`
	Stage         string     `json:"stage"`
	MoleculeID    string     `json:"molecule_id"`
	InFlight      bool       `json:"in_flight"`
	Dispatch      string     `json:"dispatch,omitempty"`
	Parked        bool       `json:"parked"`
	Reason        string     `json:"reason,omitempty"`
	Deferred      bool       `json:"deferred,omitempty"`
	DeferredUntil *time.Time `json:"deferred_until,omitempty"`
	Attempts      int        `json:"attempts"`
	AttemptLimit  int        `json:"attempt_limit"`
	// Escalation, when Escalated: the Appendix B kind, the task's reason
	// and detail, and when it was escalated.
	EscalationKind   domain.EscalationKind `json:"escalation_kind,omitempty"`
	EscalationReason string                `json:"escalation_reason,omitempty"`
	EscalationDetail string                `json:"escalation_detail,omitempty"`
	EscalatedAt      *time.Time            `json:"escalated_at,omitempty"`
	// PendingDelete is the delete waiting to retire the task, or the one
	// that failed and left it Escalated; null when there is none.
	PendingDelete *TaskPendingDelete `json:"pending_delete"`
	Parent        *engine.ParentRef  `json:"parent,omitempty"`
	Child         *engine.ChildRef   `json:"child,omitempty"`
	Edges         TaskEdges          `json:"edges"`
	Readiness     map[string]any     `json:"readiness"`
	// Trace is the ordered stages the task has been through, each with the
	// edits made while it was there; Edits lists every edit in order.
	Trace []TaskTraceEntry `json:"trace"`
	// TraceOmitted counts the oldest stage visits left out to fit the
	// response; TraceTruncated is set when the task has more transitions
	// than one read follows (20,000).
	TraceOmitted   int             `json:"trace_omitted,omitempty"`
	TraceTruncated bool            `json:"trace_truncated,omitempty"`
	Edits          []TaskEditEntry `json:"edits"`
	// EditsTruncated is set when the task has more edits than one read
	// lists (taskGetEditLimit); the newest are kept.
	EditsTruncated bool `json:"edits_truncated,omitempty"`
}

// taskGetEditLimit is how many edits one TaskGet lists: the newest, in order.
var taskGetEditLimit = 1000

// TaskSupersededFields are the declared files, test filter and test shape a
// turn in flight still runs under after an edit changed the task's structured
// fields mid-turn (daemon/TASKS.md); omitted once the turn finishes.
type TaskSupersededFields struct {
	DeclaredFiles []string `json:"declared_files"`
	TestFilter    string   `json:"test_filter"`
	TestShape     string   `json:"test_shape"`
	Dispatch      string   `json:"dispatch,omitempty"`
	Until         int      `json:"until,omitempty"`
}

// TaskPendingDelete is a TaskGet's view of the task's delete (daemon/TASKS.md):
// the operator's reason or successor, when it was requested and its status:
// `waiting-for-turn` (the task's turn in flight is being collected),
// `uncertain` (the turn in flight is parked UncertainDispatch after a restart
// and awaits resolve-uncertain), `retiring` (nothing is in flight; the next
// drive round retires it) or `failed` (the delete ended with task-delete-failed
// and the task stays Escalated for the operator). The stored pending delete's
// fields follow.
type TaskPendingDelete struct {
	Reason            string    `json:"reason"`
	SupersededBy      string    `json:"superseded_by"`
	RequestedAt       time.Time `json:"requested_at"`
	Status            string    `json:"status"`
	UncertainDispatch bool      `json:"uncertain_dispatch,omitempty"`
	Retirement        string    `json:"retirement"`
	Action            string    `json:"action,omitempty"`
	Dispatch          string    `json:"dispatch,omitempty"`
	Actor             string    `json:"actor,omitempty"`
	MessageID         string    `json:"message_id,omitempty"`
	Refused           int       `json:"refused,omitempty"`
	// Cause is why a failed delete failed.
	Cause string `json:"cause,omitempty"`
}

// taskPendingDelete views the task's delete: the stored pending one, or the
// failed one that left the Escalated task (its task-delete-failed event
// since the escalation). Nil when the task has neither.
func taskPendingDelete(e envelope, escalatedAt *time.Time, edits []TaskEditEntry) *TaskPendingDelete {
	if del := e.Deletion; del != nil {
		v := &TaskPendingDelete{RequestedAt: del.RequestedAt, Status: "retiring", Retirement: del.Retirement, Action: del.Action, Dispatch: del.Dispatch, Actor: del.Actor, MessageID: del.MessageID, Refused: del.Refused}
		if e.Job.InFlight != nil {
			if e.Job.Reason == "UncertainDispatch" {
				v.Status = "uncertain"
				v.UncertainDispatch = true
			} else {
				v.Status = "waiting-for-turn"
			}
		}
		v.Reason, v.SupersededBy = retirementReason(del.Retirement)
		return v
	}
	if e.Job.State != domain.Escalated {
		return nil
	}
	for n := len(edits) - 1; n >= 0; n-- {
		edit := edits[n]
		if edit.Kind != "task-delete-failed" || escalatedAt != nil && edit.At.Before(*escalatedAt) {
			continue
		}
		var detail struct {
			Retirement  string    `json:"retirement"`
			Cause       string    `json:"cause"`
			MessageID   string    `json:"message_id"`
			Refused     int       `json:"refused"`
			RequestedAt time.Time `json:"requested_at"`
		}
		_ = json.Unmarshal(edit.Detail, &detail)
		v := &TaskPendingDelete{RequestedAt: detail.RequestedAt, Status: "failed", Retirement: detail.Retirement, MessageID: detail.MessageID, Refused: detail.Refused, Cause: detail.Cause}
		if v.RequestedAt.IsZero() {
			v.RequestedAt = edit.At
		}
		v.Reason, v.SupersededBy = retirementReason(detail.Retirement)
		return v
	}
	return nil
}

// retirementReason splits a delete's retirement into the operator's reason
// (obsolete REASON) or its successor (superseded-by ID).
func retirementReason(retirement string) (reason, successor string) {
	class, reference, err := engine.ParseRetirement(retirement)
	switch {
	case err != nil:
		return retirement, ""
	case class == "superseded-by":
		return "", reference
	default:
		return reference, ""
	}
}

// TaskEdges are a task's edges in (its prerequisites) and out (its
// dependents).
type TaskEdges struct {
	In  []TaskEdge `json:"in"`
	Out []TaskEdge `json:"out"`
}

// TaskEdge is one edge with the state of its other end ("missing" when the
// plane has no task for it); an edge in says whether it releases the task.
type TaskEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Kind  string `json:"kind"`
	State string `json:"state"`
	// Satisfied (edges in) is whether the edge lets the task be released:
	// a relates edge always, a blocking one once its prerequisite is Done
	// or retired contained.
	Satisfied *bool `json:"satisfied,omitempty"`
}

// TaskTraceEntry is one stage visit. LeftAt is null while the task is
// still there; Outcome is the cause recorded when it left, To where it went.
type TaskTraceEntry struct {
	Stage     string          `json:"stage"`
	EnteredAt time.Time       `json:"entered_at"`
	LeftAt    *time.Time      `json:"left_at"`
	Outcome   string          `json:"outcome"`
	To        string          `json:"to,omitempty"`
	Attempt   int             `json:"attempt"`
	Edits     []TaskEditEntry `json:"edits,omitempty"`
}

// TaskEditEntry is one recorded edit of the task: a task-edited event (an
// update of its admission fields), a backlog_edge_changed event (an edge to
// or from it added or removed), or its delete (task-deleted,
// task-delete-refused, task-delete-failed). Detail is the event's payload.
type TaskEditEntry struct {
	Sequence int64           `json:"sequence"`
	At       time.Time       `json:"at"`
	Kind     string          `json:"kind"`
	Detail   json.RawMessage `json:"detail"`
}

// taskEditKinds are the event kinds TaskGet lists as edits.
var taskEditKinds = []string{"task-edited", "backlog_edge_changed", "task-deleted", "task-delete-refused", "task-delete-failed"}

// taskGet reads one task by key (FAE-CP-009): its row and snapshot, its
// backlog row, its transitions, its edit events, and the edges into and out
// of it with the other ends' rows. It reads no other task's snapshot and no
// whole table, and runs in the read pool without the writer lock.
func (d *Daemon) taskGet(ctx context.Context, raw json.RawMessage) (any, error) {
	var p TaskGetParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, errors.New("task id required")
	}
	s, err := d.store.GetJob(ctx, p.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("unknown task %s", p.ID)
	}
	if err != nil {
		return nil, err
	}
	if s.State == string(domain.Retired) && !p.IncludeRetired {
		return nil, errors.New("retired task hidden; use include_retired")
	}
	var e envelope
	if err = decodeEnvelope(s.Payload, &e); err != nil {
		return nil, err
	}
	j, a := e.Job, e.Admission
	followUpOf := j.FollowUpOf
	if followUpOf == "" {
		followUpOf = a.FollowUpOf
	}
	turnBudget := j.TurnBudget
	if turnBudget == nil {
		turnBudget = a.TurnBudget
	}
	readOnlyFiles := j.ReadOnlyFiles
	if len(readOnlyFiles) == 0 {
		readOnlyFiles = a.ReadOnlyFiles
	}
	reads := j.Reads
	if reads == nil {
		reads = a.Reads
	}
	out := TaskView{ID: s.ID, Version: s.Version, FollowUpOf: followUpOf, TurnBudget: turnBudget, Title: a.Title, Content: a.Content, Priority: a.Priority, DeclaredFiles: a.DeclaredFiles, ReadOnlyFiles: readOnlyFiles, Reads: reads, Notifications: j.Notifications, TestFilter: a.TestFilter, TestShape: a.TestShape, TaskClass: a.TaskClass,
		ContentBytes: len(a.Content), ContentSHA256: controlplane.Digest([]byte(a.Content)), WorkflowID: s.WorkflowID, WorkflowRevision: s.WorkflowRevision, TargetID: s.TargetID, TargetConfig: a.TargetConfig, TargetRevision: a.TargetRevision, TargetHash: a.TargetHash,
		Mode: string(j.Disposition.Mode), RunID: a.RunID, AdmittedAt: e.AdmittedAt, State: s.State, MoleculeID: s.MoleculeID, InFlight: j.InFlight != nil, Parked: j.Parked, Reason: j.Reason, Deferred: j.Deferred,
		Attempts: j.Attempts, AttemptLimit: j.AttemptLimit(), Parent: j.Parent, Child: j.Child, Edits: []TaskEditEntry{}}
	if out.DeclaredFiles == nil {
		out.DeclaredFiles = []string{}
	}
	if j.InFlight != nil {
		out.Dispatch = j.InFlight.ID
		if e.SupersededFields != nil && dispatchBefore(*j.InFlight, e.SupersededFields.Until) {
			files := append([]string{}, e.SupersededFields.DeclaredFiles...)
			if files == nil {
				files = []string{}
			}
			out.SupersededFields = &TaskSupersededFields{
				DeclaredFiles: files,
				TestFilter:    e.SupersededFields.TestFilter,
				TestShape:     e.SupersededFields.TestShape,
				Dispatch:      j.InFlight.ID,
				Until:         e.SupersededFields.Until,
			}
		}
	}
	if j.Deferred && !j.DeferredUntil.IsZero() {
		until := j.DeferredUntil
		out.DeferredUntil = &until
	}
	if row, found, err := d.store.BacklogItem(ctx, p.ID); err != nil {
		return nil, err
	} else if found {
		out.SourceRef, out.WriteClass = row.Source, row.WriteClass
	}
	transitions, truncated, err := d.taskTransitions(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	out.TraceTruncated = truncated
	edits, err := d.store.TaskEventsOfKinds(ctx, p.ID, taskEditKinds, taskGetEditLimit+1)
	if err != nil {
		return nil, err
	}
	if len(edits) > taskGetEditLimit {
		// The read fetched one more than the limit, so the oldest is dropped.
		edits, out.EditsTruncated = edits[1:], true
	}
	for _, ev := range edits {
		detail := ev.Payload
		if !json.Valid(detail) {
			detail, _ = json.Marshal(string(ev.Payload))
		}
		out.Edits = append(out.Edits, TaskEditEntry{Sequence: ev.Sequence, At: ev.Timestamp, Kind: ev.Kind, Detail: detail})
	}
	out.Trace = taskTrace(transitions, e.AdmittedAt, out.Edits)
	out.Stage = currentStage(e, transitions)
	if j.State == domain.Escalated {
		out.EscalationKind, out.EscalationReason, out.EscalationDetail = j.EscalationKind, j.Reason, j.EscalationDetail
		for n := len(transitions) - 1; n >= 0; n-- {
			if transitions[n].To == string(domain.Escalated) {
				at := transitions[n].CreatedAt
				out.EscalatedAt = &at
				break
			}
		}
	}
	out.PendingDelete = taskPendingDelete(e, out.EscalatedAt, out.Edits)
	if out.Edges, out.Readiness, err = d.taskEdges(ctx, s, j.Deferred); err != nil {
		return nil, err
	}
	return fitTaskView(out)
}

// taskTransitions pages one task's transitions by key, oldest first, up to
// 20,000; truncated reports more.
func (d *Daemon) taskTransitions(ctx context.Context, id string) ([]controlplane.TransitionRecord, bool, error) {
	all := []controlplane.TransitionRecord{}
	after := int64(0)
	for page := 0; page < 20; page++ {
		rows, err := d.store.TaskTransitions(ctx, id, after, 1000)
		if err != nil {
			return nil, false, err
		}
		all = append(all, rows...)
		if len(rows) < 1000 {
			return all, false, nil
		}
		after = rows[len(rows)-1].ID
	}
	more, err := d.store.TaskTransitions(ctx, id, after, 1)
	return all, len(more) > 0, err
}

// taskTrace builds the stage visits from the transitions: the first visit
// (normally Pending) starts at admission, each transition ends one visit and
// starts the next. Each edit is placed in the visit it was made during.
func taskTrace(transitions []controlplane.TransitionRecord, admitted time.Time, edits []TaskEditEntry) []TaskTraceEntry {
	first := string(domain.Pending)
	if len(transitions) > 0 {
		first = transitions[0].From
		if admitted.IsZero() {
			admitted = transitions[0].CreatedAt
		}
	}
	trace := []TaskTraceEntry{}
	current := TaskTraceEntry{Stage: first, EnteredAt: admitted}
	for _, t := range transitions {
		left := t.CreatedAt
		current.LeftAt, current.Outcome, current.To = &left, t.Note, t.To
		trace = append(trace, current)
		current = TaskTraceEntry{Stage: t.To, EnteredAt: t.CreatedAt, Attempt: t.Attempt}
	}
	trace = append(trace, current)
	for _, edit := range edits {
		// The last visit entered at or before the edit holds it.
		n := sort.Search(len(trace), func(i int) bool { return trace[i].EnteredAt.After(edit.At) }) - 1
		if n < 0 {
			n = 0
		}
		trace[n].Edits = append(trace[n].Edits, edit)
	}
	return trace
}

// currentStage is the workflow stage the task is at or was held at: its
// state when that is a stage, the stage of a turn in flight, the stage a
// Blocked task was blocked from, or for an Escalated task the stage it was
// escalated from. It is empty for a task that never reached a stage.
func currentStage(e envelope, transitions []controlplane.TransitionRecord) string {
	j := e.Job
	stage := func(s string) bool { _, ok := j.Document.Stages[s]; return ok }
	switch {
	case stage(string(j.State)):
		return string(j.State)
	case j.InFlight != nil:
		return j.InFlight.Stage
	case j.State == domain.Blocked:
		return j.BlockedFrom
	}
	for n := len(transitions) - 1; n >= 0; n-- {
		if stage(transitions[n].From) {
			return transitions[n].From
		}
	}
	return ""
}

// taskEdges reads the edges into and out of a task by key, with the other
// ends' projected rows, and the task's release readiness (FAE-CLI-015's
// readiness: state, ready, withheld_by).
func (d *Daemon) taskEdges(ctx context.Context, s controlplane.JobSnapshot, deferred bool) (TaskEdges, map[string]any, error) {
	edges := TaskEdges{In: []TaskEdge{}, Out: []TaskEdge{}}
	in, err := d.store.TaskDependencies(ctx, s.ID)
	if err != nil {
		return edges, nil, err
	}
	withheld := []string{}
	for _, e := range in {
		state := "missing"
		if e.Present {
			state = e.Prerequisite.State
		}
		satisfied := CanReleaseDependent(e.Kind == "blocking", e.Present && headSatisfied(e.Prerequisite))
		if !satisfied {
			withheld = append(withheld, e.From+": "+state)
		}
		edges.In = append(edges.In, TaskEdge{From: e.From, To: e.To, Kind: e.Kind, State: state, Satisfied: &satisfied})
	}
	out, err := d.store.TaskDependents(ctx, s.ID)
	if err != nil {
		return edges, nil, err
	}
	for _, e := range out {
		edges.Out = append(edges.Out, TaskEdge{From: e.From, To: e.To, Kind: e.Kind, State: e.DependentState})
	}
	readiness := map[string]any{"state": s.State, "ready": s.State == string(domain.Pending) && len(withheld) == 0 && !deferred, "withheld_by": withheld}
	if deferred {
		readiness["deferred"] = true
	}
	return edges, readiness, nil
}

// fitTaskView fits the view to the read limit: the oldest stage visits are
// left out first (counted in trace_omitted), then the content (described by
// its size and digest).
func fitTaskView(v TaskView) (TaskView, error) {
	for !fits(v) && len(v.Trace) > 1 {
		drop := max(1, len(v.Trace)/4)
		v.Trace, v.TraceOmitted = v.Trace[drop:], v.TraceOmitted+drop
	}
	if !fits(v) && v.Content != "" {
		v.Content, v.ContentOmitted = "", true
	}
	if !fits(v) {
		return TaskView{}, tooLarge("task view exceeds the read limit; read its history with History summary=true")
	}
	return v, nil
}

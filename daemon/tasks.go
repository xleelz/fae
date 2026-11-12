package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xleelz/fae/controlplane"
)

// The live task verbs (daemon/TASKS.md): create, update, delete and read one
// task while the daemon runs. Create and update go through backlog
// admission's validation and keyed upsert; delete composes the kernel's
// Escalate and Retire edges; read is a keyed plane read.

// TaskCreateParams is one new task for the live backlog graph: a backlog
// admission item (BacklogItemParams) and edges joining it to tasks already
// on the plane.
type TaskCreateParams struct {
	BacklogItemParams
	// Priority is the task's priority, at least 10 (FAE-CLI-002); absent
	// (null), it takes 10, as `run` does. It shadows the item's field so
	// that an explicit 0 can be refused rather than defaulted.
	Priority *int `json:"priority,omitempty"`
	// Edges each name the new task as their from or to.
	Edges []controlplane.BacklogEdge `json:"edges,omitempty"`
	// AcceptInFlight is the operator's acceptance of a new edge that holds
	// an already dispatched dependent's next release, as `backlog edge add
	// --accept-in-flight` accepts one.
	AcceptInFlight bool `json:"accept_in_flight,omitempty"`
}

// TaskUpdateParams edits one task. A field left out (or null) is unchanged.
// The fields admission pinned (workflow, target, disposition, run) may be
// named only with their current values: a change is refused.
type TaskUpdateParams struct {
	ID string `json:"id"`
	// ExpectedVersion, when set, refuses the edit unless the task is at
	// that version (TaskGet's version).
	ExpectedVersion int64     `json:"expected_version,omitempty"`
	Title           *string   `json:"title,omitempty"`
	Content         *string   `json:"content,omitempty"`
	Priority        *int      `json:"priority,omitempty"`
	Source          *string   `json:"source_ref,omitempty"`
	WriteClass      *string   `json:"write_class,omitempty"`
	DeclaredFiles   *[]string `json:"declared_files,omitempty"`
	TestFilter      *string   `json:"test_filter,omitempty"`
	TestShape       *string   `json:"test_shape,omitempty"`
	WorkflowID      *string   `json:"workflow_id,omitempty"`
	Revision        *int64    `json:"revision,omitempty"`
	TargetConfig    *string   `json:"target_config,omitempty"`
	TargetRevision  *int64    `json:"target_revision,omitempty"`
	TargetHash      *string   `json:"target_hash,omitempty"`
	Mode            *string   `json:"mode,omitempty"`
	CommitExternal  *bool     `json:"commit_external,omitempty"`
	TicketRoom      *string   `json:"ticket_room,omitempty"`
	Destination     *string   `json:"destination,omitempty"`
	PushNamespace   *string   `json:"push_namespace,omitempty"`
	RunID           *string   `json:"run_id,omitempty"`
	// AuthorizingMessage cites the operator message directing the edit.
	AuthorizingMessage string `json:"authorizing_message"`
}

// taskCreate admits one task into the live graph through backlog
// admission's validation (FAE-CLI-014, FAE-OPS-014): priority at least 10
// (an absent one takes 10, as `run` does; a given one below 10, 0 included,
// is refused as every other admission refuses it, FAE-CLI-002), nonblank
// content, the
// structured fields, edges only to tasks on the plane, no self-edge and no
// cycle (the refusal names it). An existing identity is refused: a create
// never updates. The drive releases the task like any admitted item.
func (d *Daemon) taskCreate(ctx context.Context, raw json.RawMessage, a controlplane.Audit) (any, error) {
	var p TaskCreateParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, errors.New("task id required")
	}
	switch {
	case p.Priority == nil:
		p.BacklogItemParams.Priority = 10
	case *p.Priority < 10:
		return nil, fmt.Errorf("priority %d is below 10: priority must be at least 10, larger first (an omitted priority takes 10)", *p.Priority)
	default:
		p.BacklogItemParams.Priority = *p.Priority
	}
	if len(p.Edges) > 1000 {
		return nil, errors.New("a create takes at most 1000 edges")
	}
	for _, e := range p.Edges {
		if e.From != p.ID && e.To != p.ID {
			return nil, fmt.Errorf("edge %s -> %s does not name the new task %s: edit edges between existing tasks with BacklogEdge (fae backlog edge add)", e.From, e.To, p.ID)
		}
	}
	if p.AcceptInFlight && a.Actor != "operator" {
		return nil, errors.New("only operator may accept an in-flight dependency change")
	}
	if err := validateStructuredFields(p.AdmitParams); err != nil {
		return nil, err
	}
	// A create never updates: an identity the drive holds, quarantines or
	// the plane stores in any state is refused before any preparation.
	if err := d.refuseExistingTask(ctx, p.ID); err != nil {
		return nil, fmt.Errorf("%w; TaskUpdate (fae task update) edits an existing task", err)
	}
	batch := BacklogParams{Items: []BacklogItemParams{p.BacklogItemParams}, Edges: p.Edges, AuthorizingMessage: p.AuthorizingMessage}
	saved, _, err := d.admitBacklogBatch(ctx, batch, a, backlogBatch{via: "TaskCreate", create: true, acceptInFlight: p.AcceptInFlight})
	if err != nil {
		return nil, err
	}
	created := saved[0]
	// Its readiness: the new task and its prerequisites, read by key.
	into := []controlplane.BacklogEdge{}
	jobs := map[string]controlplane.JobSnapshot{created.ID: created}
	for _, e := range p.Edges {
		if e.To != created.ID {
			continue
		}
		into = append(into, e)
		if s, ok, err := d.storedJob(ctx, e.From); err != nil {
			return nil, err
		} else if ok {
			jobs[e.From] = s
		}
	}
	edges := p.Edges
	if edges == nil {
		edges = []controlplane.BacklogEdge{}
	}
	return map[string]any{"id": created.ID, "state": created.State, "version": created.Version, "edges_added": edges, "readiness": readiness(controlplane.BacklogGraph{Edges: into}, jobs, created.ID)}, nil
}

// taskUpdate edits one non-terminal task through backlog admission's keyed
// upsert (editTask): only the fields named change, the change applies from
// the task's next stage (a turn in flight keeps the text it was dispatched
// with), and it is audited with a task-edited event naming the changed
// fields. A priority change orders the next scheduling decision, as
// Reprioritize does. An edit that changes nothing writes nothing.
func (d *Daemon) taskUpdate(ctx context.Context, raw json.RawMessage, a controlplane.Audit) (any, error) {
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("unresolved checkpoint must reconcile before task update")
	}
	var p TaskUpdateParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, errors.New("task id required")
	}
	old, err := d.liveTask(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if backlogTerminal(old.State) {
		return nil, fmt.Errorf("task %s is %s: a terminal task cannot be edited; admit the work under a new identity", p.ID, old.State)
	}
	if p.ExpectedVersion != 0 && p.ExpectedVersion != old.Version {
		return nil, fmt.Errorf("task %s is at version %d, not the expected %d: read it again (TaskGet) and retry", p.ID, old.Version, p.ExpectedVersion)
	}
	var e envelope
	if err = decodeEnvelope(old.Payload, &e); err != nil {
		return nil, err
	}
	if pinnedEdit(p, old, e) {
		return nil, errPinnedChange
	}
	row, found, err := d.store.BacklogItem(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		row = controlplane.BacklogItem{ID: p.ID, Priority: e.Admission.Priority, AcceptanceDigest: controlplane.Digest([]byte(e.Admission.Content))}
	}
	edit, err := editTask(old, e, row, taskEdit{Title: p.Title, Content: p.Content, Priority: p.Priority, Source: p.Source, WriteClass: p.WriteClass, DeclaredFiles: p.DeclaredFiles, TestFilter: p.TestFilter, TestShape: p.TestShape}, a, "TaskUpdate")
	if err != nil {
		return nil, err
	}
	result := map[string]any{"id": p.ID, "changed": edit.changed, "effect": edit.effect, "state": old.State, "version": old.Version}
	if len(edit.changed) == 0 {
		return result, nil
	}
	saved, err := d.commitBacklogBatch(ctx, a, []controlplane.BacklogAdmission{{Job: edit.snapshot, Item: edit.item}}, nil)
	if err != nil {
		return nil, err
	}
	result["state"], result["version"] = saved[0].State, saved[0].Version
	return result, nil
}

// pinnedEdit reports an update naming a pinned field with a value other
// than the task's.
func pinnedEdit(p TaskUpdateParams, old controlplane.JobSnapshot, e envelope) bool {
	differs := func(v *string, current string) bool { return v != nil && *v != current }
	differs64 := func(v *int64, current int64) bool { return v != nil && *v != current }
	return differs(p.WorkflowID, old.WorkflowID) || differs64(p.Revision, old.WorkflowRevision) || differs(p.TargetConfig, e.Admission.TargetConfig) || differs64(p.TargetRevision, e.Admission.TargetRevision) || differs(p.TargetHash, e.Admission.TargetHash) || differs(p.Mode, e.Admission.Mode) || (p.CommitExternal != nil && *p.CommitExternal != e.Admission.CommitExternal) || differs(p.TicketRoom, e.Admission.TicketRoom) || differs(p.Destination, e.Admission.Destination) || differs(p.PushNamespace, e.Admission.PushNamespace) || differs(p.RunID, e.Admission.RunID)
}

// liveTask is a task the drive holds, or the plane's row of a finished one,
// for a live task verb; a quarantined or unknown task is refused by name.
// The caller holds d.mu.
func (d *Daemon) liveTask(ctx context.Context, id string) (controlplane.JobSnapshot, error) {
	if d.quarantineHeld(id) {
		return controlplane.JobSnapshot{}, fmt.Errorf("task %s is quarantined (%s): resolve it with fae quarantine release or retire", id, d.quarantined[id])
	}
	s, ok, err := d.storedJob(ctx, id)
	if err != nil {
		return s, err
	}
	if !ok {
		return s, fmt.Errorf("unknown task %s", id)
	}
	return s, nil
}

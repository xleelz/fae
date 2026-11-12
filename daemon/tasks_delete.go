package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// TaskDeleteParams deletes one task. Delete means retire and keep history
// (operator decision, 2026-10-30): nothing is removed from the plane.
type TaskDeleteParams struct {
	ID string `json:"id"`
	// Reason is the operator's reason; it becomes the retirement's
	// `obsolete` ruling unless SupersededBy names a successor.
	Reason string `json:"reason"`
	// SupersededBy names an existing task that takes over the deleted
	// task's blocking dependents (retirement class superseded-by).
	SupersededBy string `json:"superseded_by,omitempty"`
	// ExpectedVersion, when set, refuses the delete unless the task is at
	// that version (TaskGet's version).
	ExpectedVersion    int64  `json:"expected_version,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
}

// pendingTaskDelete is a delete waiting for the task's turn in flight. The
// delete escalated the task, which holds the turn and collects it, never
// abandoning it (engine.Complete); once nothing is in flight the drive
// retires the task with this retirement (completeTaskDelete). It is stored
// in the task's row, so it survives a daemon restart.
type pendingTaskDelete struct {
	// Retirement is the retirement reason: "obsolete REASON" or
	// "superseded-by SUCCESSOR".
	Retirement string `json:"retirement"`
	// Action is the delete's outcome: "withdrawn" (the task never ran a
	// turn) or "retired".
	Action      string    `json:"action"`
	RequestedAt time.Time `json:"requested_at"`
	Actor       string    `json:"actor"`
	MessageID   string    `json:"message_id"`
	// Dispatch is the turn the delete waits for.
	Dispatch string `json:"dispatch,omitempty"`
	// Refused counts the retirements the plane refused; the third ends
	// the delete (completeTaskDelete).
	Refused int `json:"refused,omitempty"`
}

// deleteRefusal is a refusal of a delete's retirement that repeats
// identically: the kernel or the retirement's validation refused it.
type deleteRefusal struct{ err error }

func (r *deleteRefusal) Error() string { return r.err.Error() }
func (r *deleteRefusal) Unwrap() error { return r.err }

// taskDelete deletes one non-terminal task by composing the kernel's own
// edges (FAE-WF-021: Escalated leaves only by Retire or a restart): a task
// not yet Escalated is escalated, which stops it and holds any turn in
// flight, and the Escalated task is then retired. A task that never ran a
// turn is "withdrawn", any other "retired"; both end Retired, so reads hide
// it by default and its history, events and audit stay. With a turn in
// flight the retirement waits, durably, until the drive has collected the
// turn (pendingTaskDelete). Blocking dependents stay held (obsolete) or are
// re-pointed to the successor (superseded-by), as Retire does.
func (d *Daemon) taskDelete(ctx context.Context, raw json.RawMessage, a controlplane.Audit) (any, error) {
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("unresolved checkpoint must reconcile before task delete")
	}
	var p TaskDeleteParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(p.Reason)
	switch {
	case p.ID == "":
		return nil, errors.New("task id required")
	case reason == "":
		return nil, errors.New("a delete requires a reason")
	case len(p.Reason) > 4096:
		return nil, errors.New("delete reason exceeds 4096 bytes")
	case p.SupersededBy != "" && len(strings.Fields(p.SupersededBy)) != 1:
		return nil, errors.New("superseded_by names one task")
	}
	s, err := d.liveTask(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if backlogTerminal(s.State) {
		return nil, fmt.Errorf("task %s is %s: a finished task cannot be deleted; its history stays", p.ID, s.State)
	}
	if p.ExpectedVersion != 0 && p.ExpectedVersion != s.Version {
		return nil, fmt.Errorf("task %s is at version %d, not the expected %d: read it again (TaskGet) and retry", p.ID, s.Version, p.ExpectedVersion)
	}
	var e envelope
	if err = decodeEnvelope(s.Payload, &e); err != nil {
		return nil, err
	}
	if del := e.Deletion; del != nil {
		return nil, fmt.Errorf("task %s is already being deleted (requested %s): it retires once its turn in flight is collected", p.ID, del.RequestedAt.Format(time.RFC3339))
	}
	retirement, class := "obsolete "+reason, "obsolete"
	if p.SupersededBy != "" {
		retirement, class = "superseded-by "+p.SupersededBy, "superseded-by"
	}
	dependents, err := d.checkDeleteRetirement(ctx, e, retirement)
	if err != nil {
		return nil, err
	}
	held, repointed := dependents, []string{}
	if class == "superseded-by" {
		held, repointed = []string{}, dependents
	}
	action := "retired"
	if e.Job.Sequence == 0 && e.Job.InFlight == nil {
		action = "withdrawn"
	}
	result := map[string]any{"id": p.ID, "action": action, "collected_in_flight": e.Job.InFlight != nil, "held_dependents": held, "repointed_dependents": repointed, "reason_class": class, "pending": false}
	if e.Job.State != domain.Escalated || e.Job.InFlight != nil {
		var events []engine.Event
		if e.Job.State != domain.Escalated {
			// Escalate stops the task; a turn in flight is held and its
			// result collected when it returns.
			if e.Job, events, err = engine.ApplyControl(e.Job, engine.Control{Method: "Escalate", Reason: "TaskDelete: " + reason}, time.Now().UTC()); err != nil {
				return nil, err
			}
		}
		e.Deletion = &pendingTaskDelete{Retirement: retirement, Action: action, RequestedAt: time.Now().UTC(), Actor: a.Actor, MessageID: a.MessageID}
		if e.Job.InFlight != nil {
			e.Deletion.Dispatch = e.Job.InFlight.ID
		}
		if s, err = d.saveTaskCheckpoint(ctx, a, s, e, events); err != nil {
			return nil, err
		}
		if e.Job.InFlight != nil {
			result["pending"], result["state"], result["version"], result["dispatch"] = true, s.State, s.Version, e.Job.InFlight.ID
			return result, nil
		}
	}
	saved, err := d.retireDeletedTask(ctx, a, s, retirement, action)
	if err != nil {
		if e.Deletion != nil {
			return nil, fmt.Errorf("task %s was escalated for deletion and its retirement did not commit; the drive retires it: %w", p.ID, err)
		}
		return nil, err
	}
	result["state"], result["version"] = saved.State, saved.Version
	return result, nil
}

// checkDeleteRetirement validates a delete's retirement before anything is
// written: the retirement reason and successor (validateRetirement) and, for
// a supersession, each re-pointed edge (no self-edge, no cycle, named); the
// retirement's transaction validates the whole graph again. It returns the
// task's blocking dependents. A refusal is a *deleteRefusal; a plane read
// error is returned as it is.
func (d *Daemon) checkDeleteRetirement(ctx context.Context, e envelope, retirement string) ([]string, error) {
	if err := d.validateRetirement(ctx, e, retirement); err != nil {
		return nil, &deleteRefusal{err}
	}
	id := e.Job.TaskID
	dependents, err := d.store.HeldDependents(ctx, id)
	if err != nil {
		return nil, err
	}
	class, successor, _ := engine.ParseRetirement(retirement)
	if class != "superseded-by" {
		return dependents, nil
	}
	for _, dependent := range dependents {
		if dependent == successor {
			return nil, &deleteRefusal{fmt.Errorf("successor %s depends on %s: re-pointing its dependents would make %s its own prerequisite", successor, id, successor)}
		}
		if err = d.refuseBlockingCycle(ctx, controlplane.BacklogEdge{From: successor, To: dependent, Kind: "blocking"}); err != nil {
			if strings.HasPrefix(err.Error(), "blocking cycle") {
				return nil, &deleteRefusal{fmt.Errorf("re-pointing %s's dependent %s to %s: %w", id, dependent, successor, err)}
			}
			return nil, err
		}
	}
	return dependents, nil
}

// retireDeletedTask retires an Escalated task with nothing in flight for a
// delete, clearing any pending delete, with a task-deleted event. The caller
// holds d.mu.
func (d *Daemon) retireDeletedTask(ctx context.Context, audit controlplane.Audit, s controlplane.JobSnapshot, retirement, action string) (controlplane.JobSnapshot, error) {
	var e envelope
	if err := decodeEnvelope(s.Payload, &e); err != nil {
		return s, err
	}
	next, events, err := engine.ApplyControl(e.Job, engine.Control{Method: "Retire", Reason: retirement}, time.Now().UTC())
	if err != nil {
		return s, &deleteRefusal{err}
	}
	e.Job, e.Deletion = next, nil
	payload, _ := json.Marshal(map[string]any{"action": action, "retirement": retirement, "actor": audit.Actor, "message_id": audit.MessageID})
	return d.saveTaskCheckpoint(ctx, audit, s, e, events, controlplane.Event{Kind: "task-deleted", JobID: s.ID, Payload: payload})
}

// saveTaskCheckpoint commits one live task verb's checkpoint of e through
// saveOperatorCheckpoint, with the engine's events and any further plane
// events, and publishes it. The caller holds d.mu.
func (d *Daemon) saveTaskCheckpoint(ctx context.Context, audit controlplane.Audit, original controlplane.JobSnapshot, e envelope, events []engine.Event, extra ...controlplane.Event) (controlplane.JobSnapshot, error) {
	s := original
	var err error
	if s.Payload, err = json.Marshal(e); err != nil {
		return original, err
	}
	s.State = string(e.Job.State)
	s.Events = nil
	for _, event := range events {
		b, _ := json.Marshal(event)
		s.Events = append(s.Events, controlplane.Event{Kind: event.Kind, JobID: s.ID, Payload: b})
	}
	s.Events = append(s.Events, extra...)
	saved, err := d.saveOperatorCheckpoint(ctx, audit, original, s)
	if err != nil {
		return original, err
	}
	saved.Events = nil
	d.jobs[s.ID] = saved
	d.notifyEvents()
	d.publishStatus()
	d.signal()
	return saved, nil
}

// deleteCollected reports a task whose delete waited for its turn in flight
// and has nothing in flight now.
func deleteCollected(e envelope) bool {
	return e.Deletion != nil && e.Job.State == domain.Escalated && e.Job.InFlight == nil
}

// retireCollectedDeletes retires any task in ids whose pending delete has
// collected its turn in flight, even when the drive round stopped at the
// tasks-in-flight cap. The caller holds d.mu.
func (d *Daemon) retireCollectedDeletes(ids []string) error {
	for _, id := range ids {
		if d.ctx.Err() != nil || d.draining || d.pendingCheckpoint != nil {
			return nil
		}
		snapshot, ok := d.jobs[id]
		if !ok || d.active[id] || snapshot.State != string(domain.Escalated) {
			continue
		}
		var e envelope
		if decodeEnvelope(snapshot.Payload, &e) == nil && deleteCollected(e) {
			if err := d.completeTaskDelete(snapshot, e); err != nil {
				return err
			}
		}
	}
	return nil
}

// completeTaskDelete retires a task whose delete waited for its turn in
// flight, once nothing is in flight: the drive collected the held turn, or
// an uncertainty resolution ended it after a restart. The drive calls it
// under d.mu for an Escalated task with a pending delete. A plane transient
// leaves the delete pending for the next round. A refusal that repeats
// identically (the retirement's validation or the kernel), or a third
// retirement the plane refuses, ends the delete with a task-delete-failed
// event and an alert, leaving the task Escalated for the operator (retire
// it, or delete it again). It returns an error only for infrastructure, as
// taskFault does.
func (d *Daemon) completeTaskDelete(s controlplane.JobSnapshot, e envelope) error {
	del := *e.Deletion
	inputs, _ := json.Marshal(map[string]any{"id": s.ID, "retirement": del.Retirement, "action": del.Action, "collected": del.Dispatch})
	audit := controlplane.Audit{Actor: "daemon", Action: "TaskDelete", MessageID: del.MessageID, Inputs: inputs, Outcome: "ok"}
	_, err := d.checkDeleteRetirement(d.ctx, e, del.Retirement)
	if err == nil {
		if _, err = d.retireDeletedTask(d.ctx, audit, s, del.Retirement, del.Action); err == nil {
			return nil
		}
	}
	var refused *deleteRefusal
	if !errors.As(err, &refused) {
		switch classifyTaskError(d.ctx, err) {
		case outcomeInfrastructure, outcomeReloaded:
			return d.taskFault(d.ctx, s.ID, err)
		case outcomeTransient, outcomeShutdown:
			d.taskLog("task %s delete deferred: %v\n", s.ID, err)
			return nil
		}
		if d.pendingCheckpoint != nil {
			// An unverified save settles first; the next round decides again.
			return nil
		}
	}
	cause := err.Error()
	e.Deletion = &del
	e.Deletion.Refused++
	kind := "task-delete-refused"
	if refused != nil || e.Deletion.Refused >= 3 {
		// The delete ends; the task stays Escalated for the operator.
		e.Deletion, kind, audit.Outcome = nil, "task-delete-failed", "refused"
	}
	payload, _ := json.Marshal(map[string]any{"retirement": del.Retirement, "cause": cause, "message_id": del.MessageID, "refused": del.Refused + 1, "requested_at": del.RequestedAt})
	// The row first, then its line (FAE-OBS-006): a line never names a
	// refusal the plane did not record.
	err = d.logAfterCommit(func() error {
		_, err := d.saveTaskCheckpoint(d.ctx, audit, s, e, nil, controlplane.Event{Kind: kind, JobID: s.ID, Payload: payload})
		return err
	}, fmt.Sprintf("task %s %s: %s\n", s.ID, kind, cause))
	if err != nil {
		d.taskLog("task %s delete refusal not recorded: %v\n", s.ID, err)
		return nil
	}
	if kind == "task-delete-failed" {
		d.driveAlert("task-delete-failed", map[string]string{"task": s.ID, "retirement": del.Retirement, "error": cause})
	}
	return nil
}

// logAfterCommit writes line to the task log once commit has succeeded, and
// nothing when it fails (FAE-OBS-006: an event's line follows its row). It
// returns commit's error.
func (d *Daemon) logAfterCommit(commit func() error, line string) error {
	if err := commit(); err != nil {
		return err
	}
	d.taskLog("%s", line)
	return nil
}

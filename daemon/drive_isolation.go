package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// taskOutcome is the single per-task fault classification (FAE-ROB-001): a
// task-scoped error never ends the drive. Only infrastructure does.
type taskOutcome string

const (
	outcomeTransient      taskOutcome = "PlaneTransient" // nothing committed; retried next round
	outcomeStale          taskOutcome = "StaleCompletion"
	outcomeReloaded       taskOutcome = "Reloaded" // cache diverged; durable row adopted
	outcomeQuarantined    taskOutcome = "Quarantined"
	outcomeInfrastructure taskOutcome = "Infrastructure"
	outcomeShutdown       taskOutcome = "Shutdown"
	outcomeUnclassified   taskOutcome = "Unclassified"
	outcomeStepFault      taskOutcome = "StepFault" // housekeeping step skipped this round
)

// taskError marks an error raised by one task's own state at a drive point.
type taskError struct {
	Point string
	Err   error
}

func (e *taskError) Error() string { return e.Point + ": " + e.Err.Error() }
func (e *taskError) Unwrap() error { return e.Err }
func taskErr(point string, err error) error {
	if err == nil {
		return nil
	}
	return &taskError{point, err}
}

// infrastructureError is an instance-level failure (the plane cannot be
// restored); it alone ends a drive generation, which then re-arms.
type infrastructureError struct{ Err error }

func (e *infrastructureError) Error() string { return "infrastructure: " + e.Err.Error() }
func (e *infrastructureError) Unwrap() error { return e.Err }

var errStaleCompletion = errors.New("completion does not match the durable in-flight dispatch")

func classifyTaskError(ctx context.Context, err error) taskOutcome {
	var transient *controlplane.RetryableError
	var infrastructure *infrastructureError
	var task *taskError
	switch {
	case errors.As(err, &infrastructure):
		return outcomeInfrastructure
	case ctx.Err() != nil:
		return outcomeShutdown
	case errors.As(err, &transient):
		return outcomeTransient
	case errors.Is(err, errStaleCompletion):
		return outcomeStale
	case errors.Is(err, controlplane.ErrConflict):
		return outcomeReloaded
	case errors.As(err, &task):
		return outcomeQuarantined
	}
	return outcomeUnclassified
}

// taskFault runs under d.mu. It isolates err to task id and returns non-nil
// only for an instance-level failure. Every outcome is logged; quarantine is
// also an audited alert. An unclassified error is quarantined conservatively.
func (d *Daemon) taskFault(ctx context.Context, id string, err error) error {
	outcome := classifyTaskError(ctx, err)
	d.lastTaskFault = taskFaultRecord{ID: id, Outcome: outcome, Error: err.Error()}
	if d.testTaskFault != nil {
		d.testTaskFault(id, outcome, err)
	}
	switch outcome {
	case outcomeInfrastructure:
		return err
	case outcomeShutdown:
		return nil
	case outcomeTransient:
		d.taskLog("task %s deferred %s: %v\n", id, outcome, err)
		return nil
	case outcomeStale:
		d.taskLog("task %s dropped %s: %v\n", id, outcome, err)
		if d.pendingCompletion != nil && d.pendingCompletion.id == id {
			d.pendingCompletion = nil
		}
		delete(d.active, id)
		d.publishStatus()
		return nil
	case outcomeReloaded:
		if reloadErr := d.reloadTask(ctx, id); reloadErr == nil {
			d.taskLog("task %s %s after: %v\n", id, outcome, err)
			return nil
		} else {
			err = errors.Join(err, reloadErr)
		}
	}
	d.quarantine(ctx, id, err)
	return nil
}

// reloadTask adopts the durable row after a cache divergence when it validates.
func (d *Daemon) reloadTask(ctx context.Context, id string) error {
	if d.active[id] && (d.pendingCompletion == nil || d.pendingCompletion.id != id) {
		return errors.New("active dispatch cannot adopt a diverged row")
	}
	actual, err := d.store.GetJob(ctx, id)
	if err != nil {
		return err
	}
	var e envelope
	if err = decodeEnvelope(actual.Payload, &e); err != nil {
		return err
	}
	if err = engine.Validate(e.Job); err != nil {
		return err
	}
	if p := d.pendingCheckpoint; p != nil && p.Before.ID == id {
		d.pendingCheckpoint = nil
	}
	d.jobs[id] = actual
	d.publishStatus()
	return nil
}

// quarantine removes a task the drive cannot safely act on from scheduling
// until an operator verb releases or retires it (FAE-ROB-042). Its durable row
// is untouched. A dispatch still running stays active until its completion is
// dropped as stale, so release never relaunches it. The alert is best effort.
func (d *Daemon) quarantine(ctx context.Context, id string, err error) {
	if d.quarantined == nil {
		d.quarantined = map[string]string{}
	}
	reason := err.Error()
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	first := d.quarantined[id] == ""
	d.quarantined[id] = reason
	delete(d.jobs, id)
	if p := d.pendingCheckpoint; p != nil && p.Before.ID == id {
		d.pendingCheckpoint = nil
	}
	if d.pendingCompletion != nil && d.pendingCompletion.id == id {
		d.pendingCompletion = nil
		delete(d.active, id)
	}
	d.taskLog("task %s %s: %s\n", id, outcomeQuarantined, reason)
	if first && ctx.Err() == nil {
		body, _ := json.Marshal(map[string]string{"task": id, "outcome": string(outcomeQuarantined), "error": reason})
		if auditErr := d.store.ControllerAction(ctx, controlplane.Audit{Actor: "daemon", Action: "task-quarantined", Outcome: "alert"}, body, true); auditErr != nil {
			d.taskLog("task %s quarantine alert unavailable: %v\n", id, auditErr)
		}
		d.notifyEvents()
	}
	d.publishStatus()
}

func (d *Daemon) taskLog(format string, args ...any) {
	if d.log != nil {
		fmt.Fprintf(d.log, time.Now().UTC().Format(time.RFC3339Nano)+" "+format, args...)
	}
}

type taskFaultRecord struct {
	ID      string
	Outcome taskOutcome
	Error   string
}

// roundStep runs one instance-level housekeeping step of a dispatch round under
// the writer lock (FAE-ROB-001). A step may release the lock around I/O that
// touches no writer state, as idle-session maintenance does (FAE-ROB-021),
// and must hold it again when it returns. A plane transient defers it; any
// other failure skips the step this round as a StepFault, alerting once per
// episode, and the round goes on for every task. Only infrastructure ends the
// drive generation.
func (d *Daemon) roundStep(name string, step func() error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var err error
	if d.testFault != nil {
		err = d.testFault("step", name)
	}
	if err == nil {
		err = step()
	}
	return d.stepOutcome(name, err)
}

// roundRead classifies one of the round's instance-level plane reads like a
// housekeeping step: ok is false when the rest of the round must be skipped,
// and only infrastructure is returned to end the generation (FAE-ROB-001).
func (d *Daemon) roundRead(name string, err error) (ok bool, fatal error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil && d.testFault != nil {
		err = d.testFault("read", name)
	}
	return err == nil, d.stepOutcome(name, err)
}

// stepOutcome runs under d.mu: success closes the step's fault episode; a
// failure other than infrastructure is logged, recorded in status and alerted
// once per episode, and never ends the drive.
func (d *Daemon) stepOutcome(name string, err error) error {
	var infrastructure *infrastructureError
	var transient *controlplane.RetryableError
	switch {
	case err == nil:
		if _, open := d.stepFaults[name]; open {
			delete(d.stepFaults, name)
			d.taskLog("drive step %s recovered\n", name)
			d.publishStatus()
		}
		return nil
	case errors.As(err, &infrastructure):
		return err
	case d.ctx.Err() != nil:
		return nil
	case errors.As(err, &transient):
		d.taskLog("drive step %s deferred %s: %v\n", name, outcomeTransient, err)
		return nil
	}
	reason := err.Error()
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	prior, open := d.stepFaults[name]
	if d.stepFaults == nil {
		d.stepFaults = map[string]string{}
	}
	d.stepFaults[name] = reason
	d.taskLog("drive step %s skipped %s: %s\n", name, outcomeStepFault, reason)
	if !open || prior != reason {
		d.driveAlert("drive-step-fault", map[string]string{"step": name, "outcome": string(outcomeStepFault), "error": reason})
	}
	d.publishStatus()
	return nil
}

// driveAlert records one audited drive alert, best effort, under d.mu.
func (d *Daemon) driveAlert(action string, fields map[string]string) {
	body, _ := json.Marshal(fields)
	if auditErr := d.store.ControllerAction(d.ctx, controlplane.Audit{Actor: "daemon", Action: action, Outcome: "alert"}, body, true); auditErr != nil {
		d.taskLog("drive alert %s unavailable: %v\n", action, auditErr)
	}
	d.notifyEvents()
}

// dispositionErr fails a row closed when it lacks the disposition persisted at
// submission (FAE-ROB-042), naming the missing context; the quarantined row
// stays resolvable by `fae quarantine retire`.
func dispositionErr(j engine.Job) error {
	m := j.Disposition.Mode
	if CanDriveDisposition(j.State.Terminal(), m == domain.Local, m == domain.Normal) {
		return nil
	}
	return fmt.Errorf("task %s lacks its submission disposition (mode %q): the mode (Local or Normal), dry-run or live, ticket room, review destination branch and push namespace were never persisted; it is not driven as Local", j.TaskID, m)
}

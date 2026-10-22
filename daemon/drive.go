package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/workflow"
)

// drive owns every generation. Workers retain daemon lifetime contexts, so a
// generation restart never cancels, forgets, or launches an active dispatch twice.
func (d *Daemon) drive() {
	defer close(d.done)
	for {
		if d.ctx.Err() != nil {
			return
		}
		d.mu.Lock()
		d.driveAlive.Store(true)
		d.drivePending.Store(false)
		d.driveGeneration.Add(1)
		d.publishStatus()
		d.mu.Unlock()
		err := d.runDriveGeneration()
		if d.ctx.Err() != nil {
			d.driveAlive.Store(false)
			return
		}
		cause := "operator-stop"
		if err != nil {
			cause = "drive-failure"
		}
		d.mu.Lock()
		d.driveCause.Store(cause)
		d.driveAlive.Store(false)
		d.publishStatus()
		body, _ := json.Marshal(map[string]any{"cause": cause, "generation": d.driveGeneration.Load(), "error": fmt.Sprint(err)})
		_ = d.store.ControllerAction(d.ctx, controlplane.Audit{Actor: "daemon", Action: "drive-exited", Outcome: "stopped"}, body, true)
		d.mu.Unlock()
		if err != nil {
			timer := time.NewTimer(time.Second)
			select {
			case <-d.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			d.mu.Lock()
			retryErr := d.requestDriveRearm(d.ctx, controlplane.Audit{Actor: "daemon", Action: "drive-auto-rearm", Outcome: "ok"})
			d.mu.Unlock()
			if retryErr != nil {
				d.mu.Lock()
				body, _ := json.Marshal(map[string]string{"cause": cause, "error": retryErr.Error()})
				auditErr := d.store.ControllerAction(d.ctx, controlplane.Audit{Actor: "daemon", Action: "drive-rearm-refused", Outcome: "alert"}, body, true)
				if auditErr != nil {
					fmt.Fprintf(d.log, "drive rearm refused (%s): %v; audit unavailable: %v\n", cause, retryErr, auditErr)
				}
				d.notifyEvents()
				d.mu.Unlock()
			}
		}
		select {
		case <-d.ctx.Done():
			return
		case <-d.driveRearm:
		}
		d.driveStop.Store(false)
	}
}

// Recover only the drive goroutine. Effect workers remain owned by their exact
// dispatch, and any collected envelope remains pending until checkpointed.
func (d *Daemon) runDriveGeneration() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("drive panic: %v", p)
		}
	}()
	return d.driveRoundLoop()
}
func (d *Daemon) driveRoundLoop() error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if d.driveStop.Load() {
			return nil
		}
		d.mu.Lock()
		backlogErr := d.reconcileBacklogCheckpoint(d.ctx)
		d.mu.Unlock()
		if backlogErr != nil {
			return backlogErr
		}
		if d.pendingCompletion != nil {
			if err := d.collect(*d.pendingCompletion); err != nil {
				return err
			}
			d.pendingCompletion = nil
			d.signal()
		}
		if d.pendingLoop != nil {
			if err := d.collectLoop(*d.pendingLoop); err != nil {
				return err
			}
			d.pendingLoop = nil
			d.signal()
		}
		select {
		case <-d.ctx.Done():
			return nil
		case <-d.child.Done():
			if err := d.restartPlane(); err != nil {
				return err
			}
			d.signal()
		case result := <-d.loopResults:
			d.pendingLoop = &result
		case result := <-d.results:
			d.pendingCompletion = &result
		case <-ticker.C:
			d.signal()
		case <-d.wake:
			if d.ctx.Err() != nil {
				return nil
			}
			if d.driveStop.Load() {
				return nil
			}
			if err := d.dispatch(); err != nil {
				return err
			}
			d.startTicketOutbox()
			d.driveRounds.Add(1)
			d.mu.Lock()
			d.publishStatus()
			d.mu.Unlock()
		}
	}
}
func (d *Daemon) persist(ctx context.Context, s controlplane.JobSnapshot, e envelope, events []engine.Event) (controlplane.JobSnapshot, error) {
	original := s
	if err := engine.Validate(e.Job); err != nil {
		return s, err
	}
	if s.ID != e.Job.TaskID || s.WorkflowID != e.Job.Document.ID || s.WorkflowRevision != int64(e.Job.Revision) {
		return s, fmt.Errorf("checkpoint binding mismatch")
	}
	var err error
	s.Payload, err = json.Marshal(e)
	if err != nil {
		return s, err
	}
	s.State = string(e.Job.State)
	s.Events = nil
	for _, event := range events {
		raw, _ := json.Marshal(event)
		s.Events = append(s.Events, controlplane.Event{Kind: event.Kind, JobID: s.ID, Payload: raw})
	}
	checkpoint := &jobCheckpoint{Before: original, Proposed: s}
	if d.pendingCompletion != nil && d.pendingCompletion.id == s.ID {
		checkpoint.Completion = d.pendingCompletion.dispatch
	}
	d.pendingCheckpoint = checkpoint
	// This is the only durable checkpoint before launch and after collection.
	for {
		saved, err := d.store.SaveJob(ctx, controlplane.Audit{Actor: "daemon", Action: "drive checkpoint", Outcome: "ok"}, s, s.Version)
		if err == nil {
			d.pendingCheckpoint = nil
			d.lastTransition.Store(time.Now().UnixNano())
			d.notifyEvents()
			return saved, nil
		}
		if ctx.Err() != nil {
			return s, ctx.Err()
		}
		// Save may have committed even when its acknowledgement was lost. An
		// exact next version is the only authority to consume this completion.
		readCtx, cancelRead := context.WithTimeout(ctx, time.Second)
		actual, readErr := d.store.GetJob(readCtx, s.ID)
		cancelRead()
		if readErr == nil {
			expected := s
			expected.Version = s.Version + 1
			if sameJobCheckpoint(actual, expected) {
				d.pendingCheckpoint = nil
				d.notifyEvents()
				return actual, nil
			}
			if !sameJobCheckpoint(actual, original) {
				return s, controlplane.ErrConflict
			}
		}
		var transient *controlplane.RetryableError
		if errors.As(err, &transient) {
			select {
			case <-ctx.Done():
				return s, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		select {
		case <-d.child.Done():
			if restartErr := d.restartPlane(); restartErr != nil {
				return s, restartErr
			}
			// A connection loss may have happened after COMMIT. Reconcile the exact
			// proposed checkpoint before retrying; never rerun the executor.
			actual, readErr := d.store.GetJob(ctx, s.ID)
			if readErr != nil {
				return s, readErr
			}
			expected := s
			expected.Version = s.Version + 1
			if sameJobCheckpoint(actual, expected) {
				d.pendingCheckpoint = nil
				d.notifyEvents()
				return actual, nil
			}
			if !sameJobCheckpoint(actual, original) {
				return s, controlplane.ErrConflict
			}
			continue
		default:
			return s, err
		}
	}

}
func (d *Daemon) dispatch() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.reconcileBacklogCheckpoint(d.ctx); err != nil {
		return err
	}
	if d.pendingCheckpoint != nil && d.pendingCheckpoint.Control {
		if err := d.reconcileJobCheckpoint(d.ctx); err != nil {
			return err
		}
	}
	if d.draining {
		return nil
	}
	if err := d.refreshRuntimeConfig(d.ctx); err != nil {
		return err
	}
	if err := d.maintainIdleSessions(d.ctx); err != nil {
		return err
	}
	if err := d.dispatchCrons(time.Now().UTC()); err != nil {
		return err
	}
	if err := d.dispatchLoops(); err != nil {
		return err
	}
	var selector func([]workflow.Binding) (workflow.Binding, bool)
	if d.config.Live != nil {
		health, err := d.store.ProviderHealth(d.ctx)
		if err != nil {
			return err
		}
		byBinding := map[[2]string]providerhealth.Health{}
		for _, h := range health {
			byBinding[[2]string{h.Provider, h.Model}] = h
		}
		now := time.Now().UTC()
		selector = func(routes []workflow.Binding) (workflow.Binding, bool) {
			for _, route := range routes {
				if byBinding[[2]string{route.Provider, route.Model}].Eligible(now) {
					return route, true
				}
			}
			return workflow.Binding{}, false
		}
	}
	ids, orderErr := d.orderedTaskIDs()
	if orderErr != nil {
		return orderErr
	}
	graph, graphErr := d.store.Backlog(d.ctx)
	if graphErr != nil {
		return graphErr
	}
	blocked := blockedBacklog(graph, d.jobs)
	for _, id := range ids {
		if d.ctx.Err() != nil {
			return nil
		}
		if len(d.active)+len(d.loopActive) >= d.effectiveTasksInFlight() {
			return nil
		}
		if d.active[id] {
			continue
		}
		snapshot := d.jobs[id]
		var e envelope
		if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
			return err
		}
		if snapshot.State == "Pending" && len(blocked[id]) > 0 {
			continue
		}
		if err := engine.Validate(e.Job); err != nil {
			return err
		}
		next, dispatch, events, err := engine.PrepareWithRoutes(e.Job, selector)
		if e.Job.InFlight != nil && !e.Job.Parked {
			dispatch = e.Job.InFlight
		}
		if err != nil {
			return err
		}
		if dispatch == nil && len(events) == 0 {
			continue
		}
		e.Job = next
		snapshot, err = d.persist(d.ctx, snapshot, e, events)
		if err != nil {
			return err
		}
		d.jobs[id] = snapshot
		d.publishStatus()
		if dispatch == nil {
			d.signal()
			continue
		}
		if d.ctx.Err() != nil {
			return nil
		}
		d.active[id] = true
		d.publishStatus()
		d.workers.Add(1)
		go func(dispatch engine.Dispatch) {
			defer d.workers.Done()
			result, err := d.executor.Execute(d.ctx, dispatch)
			select {
			case d.results <- completion{dispatch.TaskID, dispatch.ID, result, err}:
			case <-d.ctx.Done():
			}
		}(*dispatch)
	}
	return nil
}
func (d *Daemon) collect(c completion) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pendingCheckpoint != nil && d.pendingCheckpoint.Control {
		if err := d.reconcileJobCheckpoint(d.ctx); err != nil {
			return err
		}
	}
	snapshot, ok := d.jobs[c.id]
	if !ok {
		return fmt.Errorf("completion for unknown task %s", c.id)
	}
	var e envelope
	if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
		return err
	}
	if c.err != nil {
		c.result.Detail = c.err.Error()
	}
	if c.err != nil && c.result.Class != "uncertain" && c.result.Class != "interrupted" && c.result.Class != "infrastructure-deferred" && c.result.Class != "stall-retry" && c.result.Class != "startup-retry" {
		c.result = engine.Result{DispatchID: c.dispatch, TaskID: c.id, Tree: e.Job.Tree, Class: "execution-error", Session: e.Job.InFlight.Sessions[0].ID, ProducedAt: time.Now().UTC()}
	}
	next, events, err := engine.Complete(e.Job, c.dispatch, c.result)
	if err != nil && e.Job.InFlight != nil {
		// Reject a faulty executor envelope as this task's failed turn, never as
		// authority to advance it or as a reason to stop unrelated tasks.
		rejected := engine.Result{DispatchID: e.Job.InFlight.ID, TaskID: e.Job.TaskID, Tree: e.Job.InFlight.Tree, Session: e.Job.InFlight.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: "completion-rejected"}
		next, events, err = engine.Complete(e.Job, e.Job.InFlight.ID, rejected)
	}
	if err != nil {
		return err
	}
	e.Job = next
	snapshot, err = d.persist(d.ctx, snapshot, e, events)
	if err != nil {
		return err
	}
	d.jobs[c.id] = snapshot
	delete(d.active, c.id)
	d.publishStatus()
	return nil
}

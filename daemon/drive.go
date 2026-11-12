package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/provider"
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
		if err != nil && !d.autoRearm(cause) {
			return
		}
		select {
		case <-d.ctx.Done():
			return
		case <-d.driveRearm:
		}
		d.driveStop.Store(false)
	}
}

// rearmDelay is the bounded re-arm backoff: 1 s doubling to 60 s.
func rearmDelay(attempt int) time.Duration {
	if attempt >= 6 {
		return time.Minute
	}
	return min(time.Second<<attempt, time.Minute)
}

// autoRearm re-arms a failed generation with bounded backoff (FAE-ROB-001):
// it retries a refused re-arm until a generation is scheduled, the per-cause
// budget is spent or the daemon stops, alerting once per refusal episode. It
// returns false only on shutdown.
func (d *Daemon) autoRearm(cause string) bool {
	alerted := ""
	for attempt := 0; ; attempt++ {
		timer := time.NewTimer(rearmDelay(attempt))
		select {
		case <-d.ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
		d.mu.Lock()
		var err error
		if d.testFault != nil {
			err = d.testFault("rearm", cause)
		}
		if err == nil {
			err = d.requestDriveRearm(d.ctx, controlplane.Audit{Actor: "daemon", Action: "drive-auto-rearm", Outcome: "ok"})
		}
		if err != nil && alerted != err.Error() && d.ctx.Err() == nil {
			alerted = err.Error()
			body, _ := json.Marshal(map[string]any{"cause": cause, "error": alerted, "attempt": attempt + 1, "next_retry_seconds": rearmDelay(attempt + 1).Seconds()})
			if auditErr := d.store.ControllerAction(d.ctx, controlplane.Audit{Actor: "daemon", Action: "drive-rearm-refused", Outcome: "alert"}, body, true); auditErr != nil {
				fmt.Fprintf(componentLog(d.log, "drive"), "drive rearm refused (%s): %v; audit unavailable: %v\n", cause, err, auditErr)
			}
			d.notifyEvents()
		}
		d.mu.Unlock()
		// A spent budget waits for an operator re-arm (watchdog, Appendix A).
		if err == nil || errors.Is(err, errRearmBudget) {
			return true
		}
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
		if err := d.roundStep("backlog", func() error { return d.reconcileBacklogCheckpoint(d.ctx) }); err != nil {
			return err
		}
		var retry <-chan time.Time
		results, loopResults := d.results, d.loopResults
		if d.hasPendingCompletion() {
			if err := d.collect(); err != nil {
				return err
			}
			if d.hasPendingCompletion() {
				// A plane transient keeps the completion; never read past it.
				retry = time.After(time.Second)
				results = nil
			} else {
				d.signal()
			}
		}
		if d.pendingLoop != nil {
			// A loop checkpoint fault keeps the completion and reads no other
			// loop result; a completion for no in-flight loop is dropped.
			err := d.collectLoop(*d.pendingLoop)
			if errors.Is(err, errStaleLoop) {
				d.mu.Lock()
				d.taskLog("drive dropped loop completion: %v\n", err)
				d.driveAlert("loop-completion-dropped", map[string]string{"schedule": d.pendingLoop.id, "error": err.Error()})
				d.mu.Unlock()
				err = nil
			}
			if ok, fatal := d.roundRead("loop-collect", err); fatal != nil {
				return fatal
			} else if ok {
				d.pendingLoop = nil
				d.signal()
			} else {
				retry, loopResults = time.After(time.Second), nil
			}
		}
		select {
		case <-d.ctx.Done():
			return nil
		case <-d.child.Done():
			if err := d.restartPlane(); err != nil {
				return &infrastructureError{err}
			}
			d.signal()
		case result := <-loopResults:
			d.pendingLoop = &result
		case result := <-results:
			d.mu.Lock()
			d.pendingCompletion = &result
			d.mu.Unlock()
		case <-retry:
		case <-ticker.C:
			d.signalUnlessIdle()
		case <-d.wake:
			if d.ctx.Err() != nil {
				return nil
			}
			if d.driveStop.Load() {
				return nil
			}
			if err := d.round(); err != nil {
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

// persist is the only durable checkpoint before launch and after collection.
// Its errors are typed for taskFault: a verified-uncommitted plane transient is
// a RetryableError; an unverifiable acknowledgement keeps the checkpoint
// pending (settled before any later write); a lost child that cannot restart
// is infrastructure. Retries are bounded so the writer lock is never pinned.
func (d *Daemon) persist(ctx context.Context, s controlplane.JobSnapshot, e envelope, events []engine.Event) (controlplane.JobSnapshot, error) {
	original := s
	if runningSlotState(e.Job.State) && d.workflowSlots != nil {
		if _, held := d.workflowSlots.held[e.Job.SlotHolder]; !held {
			decision, err := d.ensureWorkflowSlot(ctx, &e)
			if err != nil {
				return s, err
			}
			if !decision.Granted {
				var eventsWait []engine.Event
				e.Job, eventsWait, err = engine.WorkflowSlotWait(e.Job, decision.Reason, decision.Impossible)
				events = append(events, eventsWait...)
				if err != nil {
					return s, err
				}
			}
		}
	}
	if err := engine.Validate(e.Job); err != nil {
		return s, taskErr("persist", err)
	}
	if s.ID != e.Job.TaskID || s.WorkflowID != e.Job.Document.ID || s.WorkflowRevision != int64(e.Job.Revision) {
		return s, taskErr("persist", fmt.Errorf("checkpoint binding mismatch"))
	}
	var err error
	s.Payload, err = json.Marshal(e)
	if err != nil {
		return s, taskErr("persist", err)
	}
	s.State = string(e.Job.State)
	s.Events = nil
	for _, event := range events {
		raw, _ := json.Marshal(event)
		s.Events = append(s.Events, controlplane.Event{Kind: event.Kind, JobID: s.ID, Payload: raw})
		if event.Kind == "change-request" && event.Request != nil {
			s.Events = append(s.Events, controlplane.Event{Kind: event.Kind, JobID: event.Request.Target, Payload: raw})
		}
	}
	s.Events = append(s.Events, sessionEvents(original, e.Job, d.layout.Root)...)
	open := d.checkpointing
	// An open checkpoint's plane round trips run without the writer lock, so
	// an admission never waits for them (FAE-ROB-021); pendingCheckpoint and
	// checkpointing keep every other writer off this task meanwhile.
	unlocked := func(f func()) {
		if !open {
			f()
			return
		}
		d.mu.Unlock()
		defer d.mu.Lock()
		f()
	}
	checkpoint := &jobCheckpoint{Before: original, Proposed: s}
	if d.pendingCompletion != nil && d.pendingCompletion.id == s.ID {
		checkpoint.Completion = d.pendingCompletion.dispatch
	}
	d.pendingCheckpoint = checkpoint
	defer d.logCommittedEvents(s.Events, d.lastTransition.Load())
	expected := s
	expected.Version = s.Version + 1
	for attempt := 1; ; attempt++ {
		var saved controlplane.JobSnapshot
		var err error
		fault := d.testFault
		unlocked(func() {
			if fault != nil {
				err = fault("save", s.ID)
			}
			if err == nil {
				saved, err = d.store.SaveJob(ctx, checkpointAudit(d.driveRounds.Load()+1, s.ID), s, s.Version)
			}
		})
		if err == nil {
			d.pendingCheckpoint = nil
			d.lastTransition.Store(time.Now().UnixNano())
			d.notifyEvents()
			d.cleanupTaskDirectories(e.Job, e.Admission)
			return saved, nil
		}
		if ctx.Err() != nil {
			return s, ctx.Err()
		}
		// Save may have committed even when its acknowledgement was lost. An
		// exact next version is the only authority to consume this completion.
		var actual controlplane.JobSnapshot
		var readErr error
		unlocked(func() {
			readCtx, cancelRead := context.WithTimeout(ctx, time.Second)
			actual, readErr = d.store.GetJob(readCtx, s.ID)
			cancelRead()
		})
		if readErr == nil {
			if sameJobCheckpoint(actual, expected) {
				d.pendingCheckpoint = nil
				d.lastTransition.Store(time.Now().UnixNano())
				d.notifyEvents()
				d.cleanupTaskDirectories(e.Job, e.Admission)
				return actual, nil
			}
			// The durable row is authoritative: this checkpoint did not commit.
			d.pendingCheckpoint = nil
			if !sameJobCheckpoint(actual, original) {
				return s, controlplane.ErrConflict
			}
			var transient *controlplane.RetryableError
			if errors.As(err, &transient) {
				return s, err
			}
			return s, taskErr("save", err)
		}
		select {
		case <-d.child.Done():
			// A connection loss may have happened after COMMIT. Reconcile the
			// exact proposed checkpoint before retrying; never rerun the executor.
			if restartErr := d.restartPlane(); restartErr != nil {
				return s, &infrastructureError{restartErr}
			}
			continue
		default:
		}
		if attempt >= 3 {
			return s, &controlplane.RetryableError{Attempts: attempt, Cause: errors.Join(err, readErr)}
		}
		unlocked(func() {
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		})
		if ctx.Err() != nil {
			return s, ctx.Err()
		}
	}
}

// settleCheckpoint runs under d.mu before any later write. It reports false
// while an unverifiable checkpoint must wait; a diverged one is isolated.
func (d *Daemon) settleCheckpoint(ctx context.Context) (bool, error) {
	p := d.pendingCheckpoint
	if p == nil {
		return true, nil
	}
	err := d.reconcileJobCheckpoint(ctx)
	if err == nil {
		return true, nil
	}
	if outcome := classifyTaskError(ctx, err); outcome == outcomeTransient || outcome == outcomeShutdown {
		d.taskLog("task %s checkpoint unverified: %v\n", p.Before.ID, err)
		return false, nil
	}
	return true, d.taskFault(ctx, p.Before.ID, taskErr("checkpoint", err))
}

// dispatch runs one round. The writer lock is taken per step and per task, so
// a mutating RPC waits for at most one task's checkpoint (FAE-ROB-021).
func (d *Daemon) dispatch() error {
	d.mu.Lock()
	if d.workflowSlots != nil {
		d.workflowSlots.roundLedgerFailed = false
	}
	d.mu.Unlock()
	// After a plane restart no task acts on a checkpoint the plane has not
	// re-confirmed; an unreachable plane defers the round (FAE-CP-006).
	if err := d.revalidateCheckpoints(d.ctx); err != nil {
		d.taskLog("plane re-validation deferred: %v\n", err)
		d.withholdRound(withheldPlaneUnconfirmed, nil)
		return nil
	}
	d.mu.Lock()
	settled, err := d.settleCheckpoint(d.ctx)
	draining := d.draining
	d.mu.Unlock()
	switch {
	case err != nil:
		return err
	case !settled:
		d.withholdRound(withheldCheckpointUnsettled, nil)
		return nil
	case draining:
		d.withholdRound(withheldDraining, nil)
		return nil
	}
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"runtime-config", func() error { return d.refreshRuntimeConfig(d.ctx) }},
		{"stale-leases", func() error { d.sweepStaleLeases(); return nil }},
		{"idle-sessions", func() error { return d.maintainIdleSessions(d.ctx) }},
		{"crons", func() error { return d.dispatchCrons(time.Now().UTC()) }},
		{"loops", d.dispatchLoops},
		{"stuck-commit", func() error { return d.detectStuckCommits(d.ctx, time.Now().UTC()) }},
		{"workflow-slots", func() error { return d.reconcileWorkflowSlots(d.ctx) }},
		{"sub-workflows", func() error { return d.reconcileSubWorkflows(d.ctx) }},
		{"scenario-decisions", func() error { return d.reconcileScenarioDecisions(d.ctx) }},
	} {
		if err := d.roundStep(step.name, step.run); err != nil {
			return err
		}
	}
	options := engine.Options{RouteExhaustion: d.config.RouteExhaustion}
	if d.config.Live != nil {
		health, err := d.store.ProviderHealth(d.ctx)
		if ok, err := d.roundRead("provider-health", err); !ok {
			return d.roundReadFailed(err, nil)
		}
		byBinding := map[[2]string]providerhealth.Health{}
		for _, h := range health {
			byBinding[[2]string{h.Provider, h.Model}] = h
		}
		now := time.Now().UTC()
		// A binding the bridge refused stays skipped for its task until this
		// daemon restarts, the only way its bridge command or target
		// environment changes (FAE-RT-010, engine.RouteRefusal).
		options.RouteGeneration = d.routeRefusalGeneration()
		d.configureLiveRouteOptions(&options, byBinding, now)
	} else {
		d.configureNonLiveRouteOptions(&options)
	}
	pauses := d.loadDispatchPauses(d.ctx)
	active, graphErr := d.store.ActiveBacklog(d.ctx)
	if ok, err := d.roundRead("backlog-read", graphErr); !ok {
		return d.roundReadFailed(err, nil)
	}
	graph := controlplane.BacklogGraph{Items: active.Items}
	d.mu.Lock()
	for id, snapshot := range d.jobs {
		var e envelope
		if backlogTerminal(snapshot.State) {
			continue
		}
		if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
			d.quarantine(d.ctx, id, taskErr("decode", err))
		}
	}
	ids, orderErr := d.orderedTaskIDs(graph)
	blocked := blockedDependencies(active.Edges)
	d.mu.Unlock()
	if ok, err := d.roundRead("order", orderErr); !ok {
		return d.roundReadFailed(err, blocked)
	}
	withheld := d.withholding(blocked)
	withheld.pauses = pauses
	defer func() {
		d.mu.Lock()
		withheld.finish()
		d.mu.Unlock()
	}()
	for i, id := range ids {
		if d.ctx.Err() != nil {
			return nil
		}
		full, err := d.dispatchTask(id, options, blocked, withheld, ids[i:])
		if err != nil || full {
			return err
		}
	}
	return nil
}

// dispatchTask makes one task's decision under the writer lock. Every error is
// isolated by taskFault; only infrastructure is returned.
func (d *Daemon) dispatchTask(id string, options engine.Options, blocked map[string][]string, withheld *withheldRound, remaining []string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() != nil {
		return true, nil
	}
	if d.draining {
		withheld.rest(remaining, withheldDraining)
		return true, nil
	}
	if d.pendingCheckpoint != nil {
		// A checkpoint left unsettled earlier in this round (a transient
		// plane failure) holds every task the round has not yet decided; the
		// next round settles it first (FAE-SCH-010).
		withheld.rest(remaining, withheldCheckpointUnsettled)
		return true, nil
	}
	snapshot, ok := d.jobs[id]
	// A terminal task is never dispatched (FAE-WF-023): an idle round skips
	// it without decoding or validating its snapshot (FAE-ARC-002).
	if !ok || backlogTerminal(snapshot.State) {
		return false, nil
	}
	var slotGated *envelope
	if snapshot.State != "Pending" && snapshot.State != "Escalated" {
		var gated envelope
		if decodeEnvelope(snapshot.Payload, &gated) == nil {
			decision, err := d.ensureWorkflowSlot(d.ctx, &gated)
			if err != nil || !decision.Granted {
				if decision.Reason == "" {
					decision.Reason = "host_ledger"
				}
				withheld.add(id, decision.Reason)
				if decision.Impossible {
					next, events, e2 := engine.RefuseHostFootprint(gated.Job, decision.Reason)
					if e2 != nil {
						return false, e2
					}
					gated.Job = next
					saved, e2 := d.persist(d.ctx, snapshot, gated, events)
					if e2 == nil {
						d.jobs[id] = saved
					}
					return false, e2
				}
				return false, nil
			}
			slotGated = &gated
		}
	}
	if d.active[id] {
		var activeEnv envelope
		if decodeEnvelope(snapshot.Payload, &activeEnv) == nil &&
			!deleteCollected(activeEnv) &&
			!activeEnv.Job.State.Terminal() &&
			snapshot.State != "Escalated" &&
			(withheld == nil || !d.isTaskDispatchPaused(id, snapshot, activeEnv, withheld.pauses)) &&
			hasReadyParallelBranch(activeEnv.Job) {
			options.Priority = activeEnv.Admission.Priority
			options.Admitted = activeEnv.AdmittedAt
			if d.live != nil && d.live.guard != nil {
				options.CredentialFor = func(route workflow.Binding) string {
					return d.live.guard.resolveCredentialIdentity(engine.Session{Route: route})
				}
			}
			var extraDispatches []*engine.Dispatch
			var err error
			snapshot, activeEnv, extraDispatches, err = d.dispatchParallelBranches(id, snapshot, activeEnv, nil, options)
			if err != nil {
				return false, d.taskFault(d.ctx, id, err)
			}
			if len(extraDispatches) > 0 {
				d.publishStatus()
				if d.ctx.Err() != nil {
					return true, nil
				}
				for _, extra := range extraDispatches {
					d.acquireDispatchSession(activeEnv, *extra)
					d.launch(*extra)
				}
				return false, nil
			}
		}
		withheld.active(id)
		return false, nil
	}
	var e envelope
	if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
		return false, d.taskFault(d.ctx, id, taskErr("decode", err))
	}
	if slotGated != nil {
		e = *slotGated
	}
	if deleteCollected(e) {
		// A delete that waited for this task's turn retires it (TASKS.md).
		return false, d.completeTaskDelete(snapshot, e)
	}
	if e.Job.State.Terminal() || snapshot.State == "Escalated" {
		return false, nil
	}
	if withheld != nil && d.isTaskDispatchPaused(id, snapshot, e, withheld.pauses) {
		withheld.add(id, withheldDispatchPaused)
		return false, nil
	}
	if d.executingTasksInFlight() >= d.effectiveTasksInFlight() {
		// A collected delete retires its task regardless of the cap: it
		// dispatches nothing and frees capacity (TASKS.md).
		withheld.rest(remaining, withheldTasksInFlight)
		return true, d.retireCollectedDeletes(remaining[1:])
	}
	if snapshot.State == "Pending" {
		if len(blocked[id]) > 0 {
			withheld.add(id, withheldEdge)
			return false, nil
		}
		// Release only against the current graph: an edge may have been added
		// by an RPC since the round's snapshot.
		dependencies, changed, err := d.releaseDependencies(snapshot)
		if changed {
			return d.releaseChanged(id, withheld)
		}
		if err != nil {
			withheld.add(id, withheldEdgeUnread)
			return false, nil
		}
		if len(blockedDependencies(dependencies)[id]) > 0 {
			withheld.add(id, withheldEdge)
			return false, nil
		}
		decision, slotErr := d.ensureWorkflowSlot(d.ctx, &e)
		if slotErr != nil || !decision.Granted {
			if decision.Reason == "" {
				decision.Reason = "host_ledger"
			}
			withheld.add(id, decision.Reason)
			var previous envelope
			_ = decodeEnvelope(snapshot.Payload, &previous)
			if decision.Impossible {
				next, events, err := engine.WorkflowSlotWait(e.Job, decision.Reason, true)
				if err != nil {
					return false, err
				}
				e.Job = next
				saved, err := d.persist(d.ctx, snapshot, e, events)
				if err == nil {
					d.jobs[id] = saved
				}
				return false, err
			}
			if previous.Job.SlotWaitReason != e.Job.SlotWaitReason {
				saved, err := d.persist(d.ctx, snapshot, e, nil)
				if err != nil {
					return false, err
				}
				d.jobs[id] = saved
			}
			return false, nil
		}
		var forkErr error
		if snapshot, e, changed, forkErr = d.forkAtRelease(snapshot, e); forkErr != nil {
			return false, d.taskFault(d.ctx, id, forkErr)
		} else if changed {
			return d.releaseChanged(id, withheld)
		}
	}
	if err := errors.Join(engine.Validate(e.Job), dispositionErr(e.Job)); err != nil {
		return false, d.taskFault(d.ctx, id, taskErr("validate", err))
	}
	options.Priority = e.Admission.Priority
	options.Admitted = e.AdmittedAt
	if d.live != nil && d.live.guard != nil {
		options.CredentialFor = func(route workflow.Binding) string {
			return d.live.guard.resolveCredentialIdentity(engine.Session{Route: route})
		}
	}
	options.ValidateChangeRequest = d.validateChangeRequest
	incoming, readErr := d.changeRequestsForTask(d.ctx, e.Job.TaskID)
	if readErr != nil {
		return false, d.taskFault(d.ctx, id, readErr)
	}
	e.Job.IncomingRequests = incoming
	d.prepareDocumentChecksForJob(d.ctx, &e)
	if err := d.prepareRunStats(d.ctx, &e.Job); err != nil {
		return false, d.taskFault(d.ctx, id, err)
	}
	next, dispatch, events, err := engine.PrepareWith(e.Job, options)
	if err != nil {
		// A valid task whose stage cannot be decided escalates durably once.
		if next, events, err = engine.Fault(e.Job, err); err != nil {
			return false, d.taskFault(d.ctx, id, taskErr("prepare", err))
		}
		dispatch = nil
	}
	if e.Job.InFlight != nil && !e.Job.Parked {
		dispatch = e.Job.InFlight
	}
	if dispatch == nil && len(events) == 0 {
		withheld.parked(id, next)
		return false, nil
	}
	if d.workflowSlots == nil && snapshot.State == "Pending" && next.State != "Pending" && !d.releasesMolecule(snapshot, workflow.MaxRunningMolecules(e.Job.Document)) {
		withheld.add(id, withheldMolecule)
		return false, nil
	}
	e.Job = next
	snapshot, err = d.openCheckpoint(func() (controlplane.JobSnapshot, error) { return d.persist(d.ctx, snapshot, e, events) })
	if err != nil {
		return false, d.taskFault(d.ctx, id, err)
	}
	d.jobs[id] = snapshot
	var extraDispatches []*engine.Dispatch
	if e.Job.Fan != nil && e.Job.Fan.Parallel {
		snapshot, e, extraDispatches, err = d.dispatchParallelBranches(id, snapshot, e, dispatch, options)
		if err != nil {
			return false, d.taskFault(d.ctx, id, err)
		}
	}
	d.publishStatus()
	if dispatch == nil && len(extraDispatches) == 0 {
		d.signal()
		return false, nil
	}
	if d.ctx.Err() != nil {
		return true, nil
	}
	if dispatch != nil {
		d.acquireDispatchSession(e, *dispatch)
		d.launch(*dispatch)
	}
	for _, extra := range extraDispatches {
		d.acquireDispatchSession(e, *extra)
		d.launch(*extra)
	}
	return false, nil
}

func (d *Daemon) sessionRegistry() *provider.SessionRegistry {
	if d.sessions == nil {
		d.sessions = provider.NewSessionRegistry("")
	}
	return d.sessions
}

func (d *Daemon) acquireDispatchSession(e envelope, dispatch engine.Dispatch) {
	if dispatch.Config.Primitive != workflow.AgentTurn || len(dispatch.Sessions) == 0 {
		return
	}
	s := dispatch.Sessions[0]
	role, ok := workflow.EffectiveRole(e.Job.Document, e.Job.TaskClass, s.Role)
	if !ok {
		return
	}
	route := s.Route
	if route.Provider == "" && len(role.Routes) > 0 {
		route = role.Routes[0]
	}
	revHash, err := workflow.Hash(e.Job.Document)
	if err != nil || revHash == "" {
		revHash = fmt.Sprint(e.Job.Revision)
	}
	_, _ = d.sessionRegistry().Acquire(provider.AcquireRequest{
		TaskID:           e.Job.TaskID,
		WorkflowID:       e.Job.Document.ID,
		WorkflowRevision: revHash,
		Role:             s.Role,
		Stage:            dispatch.Stage,
		Visit:            dispatch.Visit,
		Scope:            role.Scope,
		Reuse:            role.Scope,
		Fresh:            role.Fresh,
		Provider:         route.Provider,
		Model:            route.Model,
		Binding:          route,
		Profile:          role.Profile,
		Mode:             strings.ToLower(e.Admission.Mode),
		Budget:           e.Job.TurnBudget,
		Now:              time.Now().UTC(),
	})
}

// deleteCollectedFor reports a task the drive holds whose delete waited for
// its turn and has nothing in flight now (deleteCollected). The caller holds
// d.mu.
func (d *Daemon) deleteCollectedFor(id string) bool {
	snapshot, ok := d.jobs[id]
	var e envelope
	return ok && !d.active[id] && !backlogTerminal(snapshot.State) && decodeEnvelope(snapshot.Payload, &e) == nil && deleteCollected(e)
}

// releaseDependencies reads a Pending task's dependency rows at its release
// without the writer lock (FAE-ROB-020/-021). Like a drive checkpoint, the
// read is an open checkpoint: admissions proceed, and every other writer, an
// edge edit among them, waits for it, so the release still sees the current
// graph. The task is read again under the lock; changed reports a task that
// changed meanwhile.
func (d *Daemon) releaseDependencies(s controlplane.JobSnapshot) (dependencies []controlplane.DependencyEdge, changed bool, err error) {
	d.checkpointing = true
	defer d.closeCheckpoint()
	fault := d.testFault
	func() {
		d.mu.Unlock()
		defer d.mu.Lock()
		if fault != nil {
			err = fault("plane-read", "release-dependencies/"+s.ID)
		}
		if err == nil {
			dependencies, err = d.store.TaskDependencies(d.ctx, s.ID)
		}
	}()
	now, ok := d.jobs[s.ID]
	return dependencies, !ok || now.Version != s.Version || d.ctx.Err() != nil, err
}

// releaseChanged ends a release whose task changed while the writer lock was
// released: the next round decides the task afresh (FAE-SCH-010). It runs
// under d.mu.
func (d *Daemon) releaseChanged(id string, withheld *withheldRound) (bool, error) {
	if d.ctx.Err() != nil {
		return true, nil
	}
	withheld.add(id, withheldReleaseChanged)
	d.signal()
	return false, nil
}

// launch runs under d.mu. Commit and integrate keep running through a graceful
// shutdown until the drain-down bound; every other turn stops for resume.
func (d *Daemon) launch(dispatch engine.Dispatch) {
	d.active[dispatch.TaskID] = true
	d.publishStatus()
	parent := d.ctx
	if drainProtected(dispatch.Config.Primitive) {
		parent = d.drainCtx
	}
	ctx, cancel := context.WithCancel(withWithheldTask(parent, dispatch.TaskID))
	d.registerDispatchCancel(dispatch.TaskID, dispatch.ID, cancel)
	d.workers.Add(1)
	go func() {
		defer d.workers.Done()
		defer cancel()
		result, err := d.executor.Execute(ctx, dispatch)
		d.deliver(completion{dispatch.TaskID, dispatch.ID, result, err})
	}()
}
func drainProtected(p workflow.Primitive) bool {
	return p == workflow.VcsCommit || p == workflow.Integrate
}

// deliver hands a completion to the drive, or to shutdown once it has stopped.
func (d *Daemon) deliver(c completion) {
	select {
	case d.results <- c:
	case <-d.ctx.Done():
		d.drainMu.Lock()
		d.drained = append(d.drained, c)
		d.drainMu.Unlock()
	}
}

// round is one drive round: it collects every completion already delivered,
// across all tasks, before it dispatches (FAE-SCH-009).
func (d *Daemon) round() error {
	if err := d.collectDelivered(); err != nil {
		return err
	}
	return d.dispatch()
}

// collectDelivered applies each delivered completion in turn. A completion a
// plane transient keeps pending stops the drain: it is never read past.
func (d *Daemon) collectDelivered() error {
	for {
		if d.hasPendingCompletion() {
			if err := d.collect(); err != nil {
				return err
			}
			if d.hasPendingCompletion() {
				return nil
			}
		}
		select {
		case result := <-d.results:
			d.mu.Lock()
			d.pendingCompletion = &result
			d.mu.Unlock()
		default:
			return nil
		}
	}
}

func (d *Daemon) hasPendingCompletion() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pendingCompletion != nil
}

// collect applies the pending completion. A transient keeps it pending; any
// other per-task fault is isolated; only infrastructure ends the drive.
func (d *Daemon) collect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.checkpointing = true
	defer d.closeCheckpoint()
	return d.collectPending(d.ctx)
}

// openCheckpoint runs one drive checkpoint, under d.mu, whose plane round
// trips release the writer lock (FAE-ROB-021).
func (d *Daemon) openCheckpoint(save func() (controlplane.JobSnapshot, error)) (controlplane.JobSnapshot, error) {
	d.checkpointing = true
	defer d.closeCheckpoint()
	return save()
}
func (d *Daemon) closeCheckpoint() {
	d.checkpointing = false
	if d.checkpointDone != nil {
		d.checkpointDone.Broadcast()
	}
}

// awaitCheckpoint runs under d.mu: a writer other than an admission waits for
// an open drive checkpoint instead of observing it half done.
func (d *Daemon) awaitCheckpoint() {
	for d.checkpointing && d.checkpointDone != nil {
		d.checkpointDone.Wait()
	}
}
func (d *Daemon) collectPending(ctx context.Context) error {
	settled, err := d.settleCheckpoint(ctx)
	if err != nil || !settled || d.pendingCompletion == nil {
		return err
	}
	c := *d.pendingCompletion
	if err := d.applyCompletion(ctx, c); err != nil {
		return d.taskFault(ctx, c.id, err)
	}
	d.pendingCompletion = nil
	return nil
}
func (d *Daemon) applyCompletion(ctx context.Context, c completion) error {
	snapshot, ok := d.jobs[c.id]
	if !ok {
		return fmt.Errorf("%w: unknown task %s", errStaleCompletion, c.id)
	}
	var e envelope
	if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
		return taskErr("decode", err)
	}
	inFlightDisp := e.Job.InFlight
	branchID := ""
	if e.Job.Fan != nil && e.Job.Fan.Parallel {
		if bid, bDisp, ok := matchParallelBranchDispatch(e.Job, c.dispatch); ok {
			branchID = bid
			inFlightDisp = bDisp
		}
	}
	if inFlightDisp == nil || inFlightDisp.ID != c.dispatch {
		return fmt.Errorf("%w: %s", errStaleCompletion, c.dispatch)
	}
	d.clearDispatchCancel(c.id, c.dispatch)
	if d.inFlightPreempt != nil && (d.inFlightPreempt[c.id] == c.dispatch || d.inFlightPreempt[c.dispatch] == c.dispatch) {
		delete(d.inFlightPreempt, c.id)
		delete(d.inFlightPreempt, c.dispatch)
		if c.result.Checkpoint != nil {
			e.Job.SaveDispatchCheckpoint(*inFlightDisp, *c.result.Checkpoint)
		}
		next, events, err := engine.ResumeInFlightWithNotification(e.Job, c.dispatch)
		if err != nil {
			return taskErr("preempt-in-flight", err)
		}
		e.Job = next
		snapshot, err = d.persist(ctx, snapshot, e, events)
		if err != nil {
			return err
		}
		d.jobs[c.id] = snapshot
		if branchID != "" {
			if !d.clearBranchActive(c.id, branchID) && next.InFlight == nil {
				delete(d.active, c.id)
			}
		} else {
			delete(d.active, c.id)
		}
		d.publishStatus()
		d.signal()
		return nil
	}
	if c.result.Transcript != nil && c.result.Transcript.ProviderSession != "" && len(inFlightDisp.Sessions) > 0 {
		s := inFlightDisp.Sessions[0]
		role, _ := workflow.EffectiveRole(e.Job.Document, e.Job.TaskClass, s.Role)
		revHash, err := workflow.Hash(e.Job.Document)
		if err != nil || revHash == "" {
			revHash = fmt.Sprint(e.Job.Revision)
		}
		d.sessionRegistry().BindThreadID(c.id, e.Job.Document.ID, revHash, s.Role, s.Route.Provider, s.Route.Model, strings.ToLower(e.Admission.Mode), c.result.Transcript.ProviderSession, role.Fresh, time.Now().UTC())
	}
	if c.err != nil {
		c.result.Detail = c.err.Error()
	}
	if c.err != nil && !typedFailure(c.result.Class) {
		c.result = engine.Result{DispatchID: c.dispatch, TaskID: c.id, Tree: inFlightDisp.Tree, Class: "execution-error", Session: inFlightDisp.Sessions[0].ID, ProducedAt: time.Now().UTC(), Detail: c.result.Detail}
	}
	beforeFan := e.Job.Fan
	var next engine.Job
	var events []engine.Event
	var err error
	if branchID != "" {
		next, events, err = engine.CompleteBranch(e.Job, branchID, c.dispatch, c.result)
		if err != nil {
			rejected := engine.Result{DispatchID: inFlightDisp.ID, TaskID: e.Job.TaskID, Tree: inFlightDisp.Tree, Session: inFlightDisp.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: "completion-rejected"}
			next, events, err = engine.CompleteBranch(e.Job, branchID, inFlightDisp.ID, rejected)
		}
	} else {
		next, events, err = engine.Complete(e.Job, c.dispatch, c.result)
		if err != nil {
			// Reject a faulty executor envelope as this task's failed turn, never as
			// authority to advance it or as a reason to stop unrelated tasks.
			rejected := engine.Result{DispatchID: inFlightDisp.ID, TaskID: e.Job.TaskID, Tree: inFlightDisp.Tree, Session: inFlightDisp.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: "completion-rejected"}
			next, events, err = engine.Complete(e.Job, inFlightDisp.ID, rejected)
		}
	}
	if err != nil {
		return taskErr("complete", err)
	}
	if err = d.observeStartupExhaustion(ctx, *inFlightDisp, len(c.result.Reviews), next, events); err != nil {
		return err
	}
	if beforeFan != nil {
		if err = d.integrateFanBranchesAtJoin(ctx, c.id, e.Admission.RunID, beforeFan, &next); err != nil {
			return taskErr("fan-integrate", err)
		}
	}
	e.Job = next
	snapshot, err = d.persist(ctx, snapshot, e, events)
	if err != nil {
		return err
	}
	d.jobs[c.id] = snapshot
	for _, ev := range events {
		if ev.Kind == "branch-cancelled" && ev.DispatchID != "" {
			if cancel := d.inFlightCancel[ev.DispatchID]; cancel != nil {
				cancel()
			}
			d.clearDispatchCancel(c.id, ev.DispatchID)
			if beforeFan != nil {
				for _, b := range beforeFan.Branches {
					if b.InFlight != nil && b.InFlight.ID == ev.DispatchID {
						d.clearBranchActive(c.id, b.ID)
					}
				}
			}
		}
	}
	if branchID != "" {
		if !d.clearBranchActive(c.id, branchID) && next.InFlight == nil && !d.hasActiveBranches(c.id) {
			delete(d.active, c.id)
		}
		if hasReadyParallelBranch(next) {
			d.signal()
		}
	} else {
		delete(d.active, c.id)
	}
	d.publishStatus()
	return nil
}

// typedFailure lists executor classes that carry their own engine meaning even
// when the executor also returned an error.
func typedFailure(class string) bool {
	switch class {
	case "uncertain", "interrupted", "infrastructure-deferred", "stall-retry", "startup-retry", engine.ClassStaleResidue, engine.ClassRejectedChange, engine.ClassOwnHunkLoss, engine.ClassMalformedOutput, engine.ClassValidationFailed, engine.ClassProviderFailover, engine.ClassBindingRefused, engine.ClassLaterReviewerNotStarted, engine.ClassRefusedBeforeLaunch, engine.ClassQuotaCutoff:
		return true
	}
	return false
}

// routeRefusalGeneration names this daemon process's bridge configuration:
// its process and start instant, so every daemon start begins a new one.
func (d *Daemon) routeRefusalGeneration() string {
	return fmt.Sprintf("%d@%s", os.Getpid(), d.started.Format(time.RFC3339Nano))
}

// observeStartupExhaustion records FAE-RT-004's ProviderUnavailable against the
// exhausted binding before the park is persisted. The observation identity is
// the dispatch, so a retried collection never records it twice. ran counts
// the review sessions collected before the one that failed to start
// (engine.ClassLaterReviewerNotStarted); that session's binding is charged.
// The engine counts starts per binding (engine.AgentStartCountKey), so the
// binding charged is the one whose own third start in the turn failed.
func (d *Daemon) observeStartupExhaustion(ctx context.Context, dispatch engine.Dispatch, ran int, next engine.Job, events []engine.Event) error {
	if !next.Parked || next.Reason != engine.ParkProviderUnavailable || len(events) == 0 || events[len(events)-1].DispatchID != dispatch.ID {
		return nil
	}
	route := dispatch.Sessions[0].Route
	if ran > 0 && ran < len(dispatch.Sessions) {
		route = dispatch.Sessions[ran].Route
	}
	if route.Provider == "" {
		return nil
	}
	err := d.store.ObserveProvider(ctx, controlplane.ProviderObservation{ID: dispatch.ID + "/startup-exhausted", TaskID: dispatch.TaskID, Provider: route.Provider, Model: route.Model, Outcome: providerhealth.Unavailable, At: time.Now().UTC()})
	if err != nil {
		return &controlplane.RetryableError{Attempts: 1, Cause: err}
	}
	d.notifyEvents()
	return nil
}

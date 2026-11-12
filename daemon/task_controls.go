package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/workflow"
)

type TaskControlParams struct {
	ExpectedVersion    int64     `json:"expected_version,omitempty"`
	ExpectedDigest     string    `json:"expected_digest,omitempty"`
	CensusDigest       string    `json:"census_digest,omitempty"`
	CensusEvidence     string    `json:"census_evidence,omitempty"`
	ID                 string    `json:"id"`
	Reason             string    `json:"reason,omitempty"`
	Information        string    `json:"information,omitempty"`
	Context            string    `json:"context,omitempty"`
	Until              time.Time `json:"until,omitempty"`
	Priority           int       `json:"priority,omitempty"`
	AttemptLimit       int       `json:"attempt_limit,omitempty"`
	AuthorizingMessage string    `json:"authorizing_message"`
}

func (d *Daemon) taskControl(ctx context.Context, method string, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	d.awaitCheckpoint()
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("unresolved checkpoint must reconcile before task control")
	}
	var p TaskControlParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Information) > 1<<20 || len(p.Context) > 1<<20 || len(p.Reason) > 4096 {
		return nil, errors.New("task control text exceeds bound")
	}
	s, ok, lookupErr := d.storedJob(ctx, p.ID)
	if lookupErr != nil {
		return nil, lookupErr
	}
	if !ok {
		return nil, errors.New("unknown task")
	}
	if p.ExpectedVersion != 0 || p.ExpectedDigest != "" {
		if !CanApplyCensus(s.Version == p.ExpectedVersion, controlplane.Digest(s.Payload) == p.ExpectedDigest) {
			return nil, errors.New("stale census snapshot")
		}
	}
	original := s
	var e envelope
	if err := decodeEnvelope(s.Payload, &e); err != nil {
		return nil, err
	}
	if method == "ResumeAtStage" {
		target, err := d.pinnedTarget(ctx, e.Admission)
		if err != nil {
			return nil, err
		}
		if target != nil && target.RestartOnly {
			return nil, errors.New("target policy requires full restart")
		}
	}
	if e.Deletion != nil && (method == "RestartWithInformation" || method == "ResumeAtStage" || method == "Resume" || method == "Block") {
		// A pending delete keeps the task Escalated until it retires
		// (TASKS.md); only Retire, or the delete itself, ends it.
		return nil, fmt.Errorf("task %s is being deleted: it retires once its turn in flight is collected; %s refused", p.ID, method)
	}
	held := []string{}
	if method == "Cancel" || method == "Retire" {
		var err error
		held, err = d.store.HeldDependents(ctx, p.ID)
		if err != nil {
			return nil, err
		}
	}
	if method == "Retire" {
		if err := d.validateRetirement(ctx, e, p.Reason); err != nil {
			return nil, err
		}
	}
	next, events, err := engine.ApplyControl(e.Job, engine.Control{Method: method, Reason: p.Reason, Information: p.Information, Context: p.Context, Until: p.Until, Priority: p.Priority, AttemptLimit: p.AttemptLimit, RouteGeneration: d.routeRefusalGeneration()}, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if d.live != nil && next.State != e.Job.State && (method == "RestartWithInformation" || method == "ResumeAtStage" || method == "Resume") {
		if next.ExecutionHead, err = d.live.checkControlWorkspace(ctx, e); err != nil {
			return nil, err
		}
	}
	restarted := (method == "RestartWithInformation" || method == "ResumeAtStage" || method == "Resume") && e.Job.State == domain.Escalated
	e.Job = next
	if next.State.Terminal() {
		e.Deletion = nil // a retirement ends a pending delete
	}
	if method == "Reprioritize" {
		e.Admission.Priority = p.Priority
	}
	s.Payload, err = json.Marshal(e)
	if err != nil {
		return nil, err
	}
	s.State = string(next.State)
	s.Events = nil
	for _, event := range events {
		b, _ := json.Marshal(event)
		s.Events = append(s.Events, controlplane.Event{Kind: event.Kind, JobID: s.ID, Payload: b})
	}
	saved, err := d.saveOperatorCheckpoint(ctx, audit, original, s)
	if err != nil {
		return nil, err
	}
	d.jobs[p.ID] = saved
	d.notifyEvents()
	d.publishStatus()
	d.signal()
	result := map[string]any{"id": p.ID, "state": saved.State, "version": saved.Version, "held_dependents": held}
	var waiting envelope
	if decodeEnvelope(saved.Payload, &waiting) == nil && waiting.Job.SlotWaitReason != "" {
		result["waiting"] = true
		result["withheld_reason"] = waiting.Job.SlotWaitReason
	}
	if restarted {
		// A restart from the first stage re-grants at most attempts; the
		// reply states every budget the new pass draws on (FAE-WF-074).
		carried := engine.CarriedBudgets(next)
		result["carried"] = &carried
	}
	if method == "Retire" {
		class, reference, _ := engine.ParseRetirement(p.Reason)
		result["reason_class"], result["reference"], result["dependents"] = class, reference, held
		if class != "obsolete" {
			result["held_dependents"] = []string{}
		}
		if class == "superseded-by" {
			result["dependent_effect"] = "transferred"
		} else if class == "contained" {
			result["dependent_effect"] = "released"
		} else {
			result["dependent_effect"] = "held"
		}
	}
	return result, nil
}

// orderedTaskIDs orders the tasks for one drive round by priority, larger
// first, then admission time (FAE-SCH-003); a task ID breaks exact ties and
// follows an unknown admission time. It publishes the round's scheduling
// view to live admission.
func (d *Daemon) orderedTaskIDs(graph controlplane.BacklogGraph) ([]string, error) {
	view, err := newScheduleView(d.jobs)
	if err != nil {
		return nil, err
	}
	// Every task records its admission time; a backlog item's stands in for
	// a task admitted before that was recorded.
	admitted := map[string]time.Time{}
	for _, item := range graph.Items {
		admitted[item.ID] = item.Admitted
	}
	for id, at := range view.admitted {
		if !at.IsZero() {
			admitted[id] = at
		}
	}
	ids := make([]string, 0, len(d.jobs))
	for id := range d.jobs {
		ids = append(ids, id)
	}
	priority := view.priority
	cutOff := view.cutOffByQuota
	sort.Slice(ids, func(i, j int) bool {
		a, b := ids[i], ids[j]
		if cutOff[a] != cutOff[b] {
			return cutOff[a]
		}
		if priority[a] != priority[b] {
			return priority[a] > priority[b]
		}
		if ta, tb := admitted[a], admitted[b]; !ta.Equal(tb) {
			return !ta.IsZero() && (tb.IsZero() || ta.Before(tb))
		}
		return a < b
	})
	if d.live != nil && d.live.guard != nil {
		d.live.guard.view.Store(view)
	}
	return ids, nil
}

// Restart preserves the original reconciliation base and requires the exact
// broker-proved execution HEAD and durable tree before changing task state.
func (x *liveExecutor) checkControlWorkspace(ctx context.Context, e envelope) (string, error) {
	w := x.manager.Binding(e.Admission.RunID, e.Job.TaskID)
	if err := x.manager.Verify(ctx, w); err != nil {
		return "", err
	}
	head, err := effectGit(ctx, w, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	expected := e.Job.ExpectedHead()
	if e.Job.ExecutionHead == "" && head != expected && head == e.Job.LatestCommitHead() {
		// Upgrade legacy snapshots only from validated broker receipt ancestry
		// and unchanged tree, never by adopting an arbitrary branch tip.
		if _, err := effectGit(ctx, w, "merge-base", "--is-ancestor", e.Job.Base, head); err == nil {
			expected = head
		}
	}
	if !CanRestartWorkspace(head == expected, true) {
		return "", errors.New("restart/resume refused: task HEAD differs from broker-proved execution HEAD")
	}
	tree, err := x.broker.Tree(ctx, w)
	if err != nil {
		return "", err
	}
	if !CanRestartWorkspace(head == expected, tree == e.Job.Tree) {
		return "", errors.New("restart/resume refused: task workspace tree differs from durable checkpoint")
	}
	return head, nil
}

// saveOperatorCheckpoint commits one operator verb's checkpoint. The row is
// validated first, as the drive, rearm and start validate every row: an
// operator verb never stores a task the drive would quarantine, and a refused
// row leaves nothing saved and no checkpoint pending.
func (d *Daemon) saveOperatorCheckpoint(ctx context.Context, audit controlplane.Audit, original, s controlplane.JobSnapshot) (controlplane.JobSnapshot, error) {
	var proposed envelope
	if err := decodeEnvelope(s.Payload, &proposed); err != nil {
		return s, fmt.Errorf("operator checkpoint refused: %w", err)
	}
	if runningSlotState(proposed.Job.State) && d.workflowSlots != nil {
		decision, err := d.ensureWorkflowSlot(ctx, &proposed)
		if err != nil {
			return s, err
		}
		if !decision.Granted {
			var waitEvents []engine.Event
			proposed.Job, waitEvents, err = engine.WorkflowSlotWait(proposed.Job, decision.Reason, decision.Impossible)
			if err != nil {
				return s, err
			}
			for _, event := range waitEvents {
				raw, _ := json.Marshal(event)
				s.Events = append(s.Events, controlplane.Event{Kind: event.Kind, JobID: s.ID, Payload: raw})
			}
		}
		s.Payload, _ = json.Marshal(proposed)
		s.State = string(proposed.Job.State)
	}
	if err := engine.Validate(proposed.Job); err != nil {
		return s, fmt.Errorf("operator checkpoint refused: invalid task %s: %w", s.ID, err)
	}
	d.pendingCheckpoint = &jobCheckpoint{Before: original, Proposed: s, Control: true}
	saved, err := d.store.SaveJob(ctx, audit, s, s.Version)
	if err != nil {
		// A commit can succeed while its acknowledgement is lost. Only the exact
		// proposed next version may update the cache; never repeat the control.
		actual, readErr := d.store.GetJob(ctx, s.ID)
		expected := s
		expected.Version++
		if readErr != nil || !sameJobCheckpoint(actual, expected) {
			if readErr == nil && sameJobCheckpoint(actual, original) {
				d.pendingCheckpoint = nil
			}
			d.signal()
			return s, err
		}
		saved = actual
	}
	d.pendingCheckpoint = nil
	d.logParksAndEscalations(s.Events)
	return saved, nil
}

func (d *Daemon) resolveScriptedOperatorHold(ctx context.Context, p ResolveUncertainParams, audit controlplane.Audit, s controlplane.JobSnapshot, e envelope) (any, error) {
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("unresolved checkpoint must reconcile first")
	}
	if d.active[p.ID] || e.Job.ExecutionKind != "scripted" || p.Action != "escalate" || p.Result != nil {
		return nil, errors.New("scripted operator hold permits only retired-dispatch escalation")
	}
	next, events, err := engine.ResolveUncertain(e.Job, p.DispatchID, p.Action, nil)
	if err != nil {
		return nil, err
	}
	return d.saveUncertainResolution(ctx, audit, p, s, e, next, events)
}

// TaskNotifyParams is the request payload for the Notify RPC (FAE-OPS-033).
type TaskNotifyParams struct {
	Task               string `json:"task"`
	FromTask           string `json:"from_task,omitempty"`
	Kind               string `json:"kind"`
	Delivery           string `json:"delivery,omitempty"`
	Body               string `json:"body"`
	Actor              string `json:"actor,omitempty"`
	Reason             string `json:"reason,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
}

// TaskNotifyRequest aliases TaskNotifyParams.
type TaskNotifyRequest = TaskNotifyParams

// TaskNotifyResponse is the response returned by the Notify RPC (FAE-OPS-033).
type TaskNotifyResponse struct {
	ID           string                  `json:"id"`
	Task         string                  `json:"task"`
	FromTask     string                  `json:"from_task,omitempty"`
	Kind         string                  `json:"kind"`
	Delivery     string                  `json:"delivery"`
	RelayedBy    string                  `json:"relayed_by,omitempty"`
	Quoted       string                  `json:"quoted"`
	InFlight     bool                    `json:"in_flight"`
	Notification engine.TaskNotification `json:"notification"`
	Version      int64                   `json:"version"`
}

// RelayNote allows the controller/daemon to relay a structured note from
// fromTask to targetTask without requiring a new operator message (FAE-OPS-033).
func (d *Daemon) RelayNote(ctx context.Context, targetTask, fromTask, body, delivery string) (TaskNotifyResponse, error) {
	if strings.TrimSpace(fromTask) == "" {
		return TaskNotifyResponse{}, errors.New("controller relay requires from_task")
	}
	if delivery == "" {
		delivery = "next-turn"
	}
	req := TaskNotifyParams{
		Task:     targetTask,
		FromTask: fromTask,
		Kind:     "note",
		Delivery: delivery,
		Body:     body,
		Actor:    "controller",
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return TaskNotifyResponse{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.taskNotify(ctx, raw, controlplane.Audit{Actor: "controller", Action: "Notify", Inputs: raw, Outcome: "ok"})
	if err != nil {
		return TaskNotifyResponse{}, err
	}
	out, _ := res.(TaskNotifyResponse)
	return out, nil
}

func (d *Daemon) taskNotify(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	d.awaitCheckpoint()
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("unresolved checkpoint must reconcile before task notify")
	}
	var p TaskNotifyParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	p.Task = strings.TrimSpace(p.Task)
	p.FromTask = strings.TrimSpace(p.FromTask)
	if p.Task == "" {
		return nil, errors.New("task id required")
	}
	if p.Delivery == "" {
		p.Delivery = "next-turn"
	}
	if !engine.ValidNotificationKind(p.Kind) {
		return nil, fmt.Errorf("invalid notification kind %q: must be breaking-change, cross-area-need, or note", p.Kind)
	}
	if !engine.ValidNotificationDelivery(p.Delivery) {
		return nil, fmt.Errorf("invalid notification delivery %q: must be next-turn or in-flight", p.Delivery)
	}
	if strings.TrimSpace(p.Body) == "" {
		return nil, errors.New("notification body required")
	}
	if len(p.Body) > 1<<20 {
		return nil, errors.New("notification body exceeds 1 MiB")
	}
	if p.FromTask != "" {
		if p.FromTask == p.Task {
			return nil, errors.New("from_task must differ from target task")
		}
		if _, ok, err := d.storedJob(ctx, p.FromTask); err != nil {
			return nil, err
		} else if !ok {
			return nil, fmt.Errorf("unknown from_task %s", p.FromTask)
		}
	}
	s, err := d.liveTask(ctx, p.Task)
	if err != nil {
		return nil, err
	}
	original := s
	var e envelope
	if err := decodeEnvelope(s.Payload, &e); err != nil {
		return nil, err
	}
	relayedBy := ""
	if audit.Actor == "controller" || p.Actor == "controller" || (p.Kind == "note" && p.FromTask != "" && strings.TrimSpace(p.AuthorizingMessage) == "") {
		relayedBy = "controller"
	}
	now := time.Now().UTC()
	nextJob, notif, events, err := engine.ApplyNotify(e.Job, engine.TaskNotification{
		TaskID:     p.Task,
		FromTaskID: p.FromTask,
		Kind:       p.Kind,
		Delivery:   p.Delivery,
		Body:       p.Body,
		RelayedBy:  relayedBy,
	}, now)
	if err != nil {
		return nil, err
	}
	inFlight := d.active[p.Task] || e.Job.InFlight != nil
	e.Job = nextJob
	s.Payload, err = json.Marshal(e)
	if err != nil {
		return nil, err
	}
	s.State = string(nextJob.State)
	s.Events = nil
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		s.Events = append(s.Events, controlplane.Event{Kind: ev.Kind, JobID: s.ID, Payload: b})
	}
	saved, err := d.saveOperatorCheckpoint(ctx, audit, original, s)
	if err != nil {
		return nil, err
	}
	d.jobs[p.Task] = saved

	// Record in task_notifications and in from_task's history/events when FromTaskID is set.
	if p.FromTask != "" {
		if fromSnap, ok, err := d.storedJob(ctx, p.FromTask); err == nil && ok && !backlogTerminal(fromSnap.State) {
			var fromEnv envelope
			if decodeEnvelope(fromSnap.Payload, &fromEnv) == nil {
				if updatedFrom, fromEvents, err := engine.RecordOutboundNotification(fromEnv.Job, notif); err == nil {
					fromOrig := fromSnap
					fromEnv.Job = updatedFrom
					if payload, err := json.Marshal(fromEnv); err == nil {
						fromSnap.Payload = payload
						fromSnap.Events = nil
						for _, ev := range fromEvents {
							b, _ := json.Marshal(ev)
							fromSnap.Events = append(fromSnap.Events, controlplane.Event{Kind: ev.Kind, JobID: fromSnap.ID, Payload: b})
						}
						if savedFrom, err := d.saveOperatorCheckpoint(ctx, audit, fromOrig, fromSnap); err == nil {
							d.jobs[p.FromTask] = savedFrom
						}
					}
				}
			}
		}
	}
	_ = d.store.Mutate(ctx, audit, func(ctx context.Context, tx *sql.Tx) error {
		return d.store.RecordTaskNotificationTx(ctx, tx, notif)
	})

	if notif.Delivery == "in-flight" {
		if d.layout.Root != "" {
			_, _ = provider.WriteLiveNotification(d.layout.Root, p.Task, notif)
			_, _ = provider.WriteLiveNotification(filepath.Join(d.layout.Root, "sessions"), p.Task, notif)
			_, _ = provider.WriteLiveNotification(filepath.Join(d.layout.Root, "provider-sessions"), p.Task, notif)
		}
		if inFlight && e.Job.InFlight != nil {
			if d.inFlightPreempt == nil {
				d.inFlightPreempt = map[string]string{}
			}
			d.inFlightPreempt[p.Task] = e.Job.InFlight.ID
			if cancel, ok := d.inFlightCancel[p.Task]; ok && cancel != nil {
				cancel()
			}
		}
	}
	d.notifyEvents()
	d.publishStatus()
	d.signal()
	return TaskNotifyResponse{
		ID:           notif.ID,
		Task:         notif.TaskID,
		FromTask:     notif.FromTaskID,
		Kind:         notif.Kind,
		Delivery:     notif.Delivery,
		RelayedBy:    notif.RelayedBy,
		Quoted:       provider.FormatNotificationsQuoted([]engine.TaskNotification{notif}),
		InFlight:     inFlight,
		Notification: notif,
		Version:      saved.Version,
	}, nil
}

// TurnBudgetSpec aliases engine.TurnBudgetSpec for daemon callers (FAE-OPS-034).
type TurnBudgetSpec = engine.TurnBudgetSpec

// TaskFollowUpParams is the request payload for the FollowUp RPC (FAE-OPS-034).
type TaskFollowUpParams struct {
	Task               string                 `json:"task"`
	ID                 string                 `json:"id,omitempty"`
	Text               string                 `json:"text"`
	Scope              []string               `json:"scope,omitempty"`
	Budget             *engine.TurnBudgetSpec `json:"budget,omitempty"`
	WallSeconds        int                    `json:"wall_seconds,omitempty"`
	MaxTurns           int                    `json:"max_turns,omitempty"`
	Actor              string                 `json:"actor,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	AuthorizingMessage string                 `json:"authorizing_message"`
}

// TaskFollowUpRequest aliases TaskFollowUpParams.
type TaskFollowUpRequest = TaskFollowUpParams

// TaskFollowUpResponse is the response returned by the FollowUp RPC (FAE-OPS-034).
type TaskFollowUpResponse struct {
	ID                string                 `json:"id"`
	FollowUpOf        string                 `json:"follow_up_of"`
	State             string                 `json:"state"`
	Version           int64                  `json:"version"`
	WorkflowID        string                 `json:"workflow_id"`
	WorkflowRevision  int64                  `json:"workflow_revision"`
	DeclaredFiles     []string               `json:"declared_files,omitempty"`
	ReadOnlyFiles     []string               `json:"read_only_files,omitempty"`
	Reads             *workflow.ReadPolicy   `json:"reads,omitempty"`
	Budget            *engine.TurnBudgetSpec `json:"budget,omitempty"`
	ProviderSessionID string                 `json:"provider_session_id,omitempty"`
	ThreadID          string                 `json:"thread_id,omitempty"`
}

func (d *Daemon) taskFollowUp(ctx context.Context, key string, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	d.awaitCheckpoint()
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("unresolved checkpoint must reconcile before follow-up")
	}
	if d.draining {
		return nil, errors.New("draining: admissions paused until restart")
	}
	var p TaskFollowUpParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	p.Task = strings.TrimSpace(p.Task)
	p.ID = strings.TrimSpace(p.ID)
	if p.Task == "" {
		return nil, errors.New("predecessor task id required")
	}
	if strings.TrimSpace(p.Text) == "" {
		return nil, errors.New("follow-up text required")
	}
	if p.Budget == nil && (p.WallSeconds > 0 || p.MaxTurns > 0) {
		p.Budget = &engine.TurnBudgetSpec{WallSeconds: p.WallSeconds, MaxTurns: p.MaxTurns}
	}
	if p.Budget != nil && (p.Budget.WallSeconds < 0 || p.Budget.MaxTurns < 0) {
		return nil, errors.New("follow-up turn budget values must be non-negative")
	}
	oldSnap, err := d.liveTask(ctx, p.Task)
	if err != nil {
		return nil, err
	}
	if oldSnap.State != string(domain.Done) {
		return nil, &api.RPCError{
			Code:    "follow-up-refused",
			Message: fmt.Sprintf("follow-up-refused: task %s is in state %s; FollowUp requires a Done task", p.Task, oldSnap.State),
		}
	}
	var oldEnv envelope
	if err := decodeEnvelope(oldSnap.Payload, &oldEnv); err != nil {
		return nil, err
	}
	// Check whether any implementer / agent_turn role (or declared role) has fresh: true (FAE-OPS-034).
	stageIDs := make([]string, 0, len(oldEnv.Job.Document.Stages))
	for id := range oldEnv.Job.Document.Stages {
		stageIDs = append(stageIDs, id)
	}
	sort.Strings(stageIDs)
	for _, stageID := range stageIDs {
		st := oldEnv.Job.Document.Stages[stageID]
		if st.Primitive == workflow.AgentTurn && st.Role != "" {
			if role, ok := workflow.EffectiveRole(oldEnv.Job.Document, oldEnv.Job.TaskClass, st.Role); ok {
				if err := provider.RefuseFollowUpIfFresh(st.Role, role); err != nil {
					return nil, &api.RPCError{Code: "follow-up-refused", Message: err.Error()}
				}
			}
		}
	}
	roleNames := make([]string, 0, len(oldEnv.Job.Document.Roles))
	for name := range oldEnv.Job.Document.Roles {
		roleNames = append(roleNames, name)
	}
	sort.Strings(roleNames)
	for _, name := range roleNames {
		if err := provider.RefuseFollowUpIfFresh(name, oldEnv.Job.Document.Roles[name]); err != nil {
			return nil, &api.RPCError{Code: "follow-up-refused", Message: err.Error()}
		}
	}

	newID := p.ID
	if newID == "" {
		for seq := 1; ; seq++ {
			candidate := fmt.Sprintf("%s-followup-%d", p.Task, seq)
			if err := d.refuseExistingTask(ctx, candidate); err == nil {
				newID = candidate
				break
			}
		}
	} else if err := d.refuseExistingTask(ctx, newID); err != nil {
		return nil, err
	}

	// Ensure the predecessor task's session record is loaded in the registry before transferring.
	now := time.Now().UTC()
	reg := d.sessionRegistry()
	defaultRevHash := d.workflowRevisionHash(oldEnv.Job.Document, int64(oldEnv.Job.Revision))
	if _, ok := reg.GetTaskSession(p.Task); !ok {
		if dbRec, found, err := d.store.GetTaskProviderSession(ctx, p.Task); err == nil && found {
			revHash := dbRec.WorkflowRevision
			if revHash == "" {
				revHash = defaultRevHash
			}
			reg.BindThreadID(p.Task, dbRec.WorkflowID, revHash, dbRec.Role, dbRec.Provider, dbRec.Model, dbRec.Mode, dbRec.ThreadID, dbRec.Fresh, now)
		} else {
			roleName := ""
			var route workflow.Binding
			var fresh bool
			for _, pRec := range oldEnv.Job.Provenance {
				if pRec.Primitive == workflow.AgentTurn && pRec.Role != "" {
					roleName = pRec.Role
					route = workflow.Binding{Provider: pRec.Provider, Model: pRec.Model}
					if r, ok := workflow.EffectiveRole(oldEnv.Job.Document, oldEnv.Job.TaskClass, roleName); ok {
						fresh = r.Fresh
					}
					if pRec.Transcript != nil && pRec.Transcript.ProviderSession != "" {
						reg.BindThreadID(p.Task, oldEnv.Job.Document.ID, defaultRevHash, roleName, route.Provider, route.Model, strings.ToLower(oldEnv.Admission.Mode), pRec.Transcript.ProviderSession, fresh, now)
					}
					break
				}
			}
			if _, ok := reg.GetTaskSession(p.Task); !ok {
				if roleName == "" && len(roleNames) > 0 {
					roleName = roleNames[0]
				}
				reg.BindThreadID(p.Task, oldEnv.Job.Document.ID, defaultRevHash, roleName, route.Provider, route.Model, strings.ToLower(oldEnv.Admission.Mode), "", fresh, now)
			}
		}
	}
	transferred, err := reg.TransferTaskSession(p.Task, newID, now)
	if err != nil {
		return nil, &api.RPCError{Code: "follow-up-refused", Message: err.Error()}
	}
	if transferred.WorkflowRevision == "" {
		transferred.WorkflowRevision = defaultRevHash
	}
	if d.layout.Root != "" {
		if role, ok := workflow.EffectiveRole(oldEnv.Job.Document, oldEnv.Job.TaskClass, transferred.Role); ok {
			var route workflow.Binding
			if len(role.Routes) > 0 {
				route = role.Routes[0]
			}
			for _, sessionRoot := range []string{
				filepath.Join(d.layout.Root, "sessions"),
				filepath.Join(d.layout.Root, "provider-sessions"),
			} {
				if diskThread, err := provider.TransferTaskSessionOnDisk(sessionRoot, transferred.WorkflowID, transferred.WorkflowRevision, transferred.Role, route, role.Profile, transferred.Mode, p.Task, newID); err == nil && diskThread != "" {
					transferred = reg.BindThreadID(newID, transferred.WorkflowID, transferred.WorkflowRevision, transferred.Role, transferred.Provider, transferred.Model, transferred.Mode, diskThread, false, now)
				}
			}
		}
	}

	declaredFiles := append([]string(nil), oldEnv.Admission.DeclaredFiles...)
	for _, f := range oldEnv.Job.ApprovedScope {
		found := false
		for _, existing := range declaredFiles {
			if existing == f {
				found = true
				break
			}
		}
		if !found && DeclaredPathSafe(f) {
			declaredFiles = append(declaredFiles, f)
		}
	}
	if len(p.Scope) > 0 {
		scoped := make([]string, 0, len(p.Scope))
		for _, f := range p.Scope {
			if !DeclaredPathSafe(f) {
				return nil, fmt.Errorf("unsafe follow-up scope path %q", f)
			}
			scoped = append(scoped, f)
		}
		declaredFiles = scoped
	}
	readOnlyFiles := append([]string(nil), oldEnv.Job.ReadOnlyFiles...)
	if len(readOnlyFiles) == 0 && len(oldEnv.Admission.ReadOnlyFiles) > 0 {
		readOnlyFiles = append([]string(nil), oldEnv.Admission.ReadOnlyFiles...)
	}
	var reads *workflow.ReadPolicy
	if src := oldEnv.Job.Reads; src != nil {
		reads = &workflow.ReadPolicy{Allow: append([]string(nil), src.Allow...), Deny: append([]string(nil), src.Deny...)}
	} else if src := oldEnv.Admission.Reads; src != nil {
		reads = &workflow.ReadPolicy{Allow: append([]string(nil), src.Allow...), Deny: append([]string(nil), src.Deny...)}
	}
	priority := oldEnv.Admission.Priority
	if priority < 10 {
		priority = 10
	}
	admitParams := AdmitParams{
		ID:                 newID,
		FollowUpOf:         p.Task,
		TurnBudget:         p.Budget,
		WorkflowID:         oldSnap.WorkflowID,
		Revision:           oldSnap.WorkflowRevision,
		TargetConfig:       oldEnv.Admission.TargetConfig,
		TargetRevision:     oldEnv.Admission.TargetRevision,
		TargetHash:         oldEnv.Admission.TargetHash,
		CommitExternal:     oldEnv.Admission.CommitExternal,
		DryRun:             oldEnv.Admission.DryRun,
		TicketRoom:         oldEnv.Admission.TicketRoom,
		Destination:        oldEnv.Admission.Destination,
		PushNamespace:      oldEnv.Admission.PushNamespace,
		Tree:               oldEnv.Job.Tree,
		Title:              oldEnv.Admission.Title,
		Content:            p.Text,
		Priority:           priority,
		RunID:              oldEnv.Admission.RunID,
		Mode:               oldEnv.Admission.Mode,
		TaskClass:          oldEnv.Admission.TaskClass,
		DeclaredFiles:      declaredFiles,
		ReadOnlyFiles:      readOnlyFiles,
		Reads:              reads,
		TestFilter:         oldEnv.Admission.TestFilter,
		TestShape:          oldEnv.Admission.TestShape,
		AuthorizingMessage: p.AuthorizingMessage,
	}
	admitRaw, err := json.Marshal(admitParams)
	if err != nil {
		return nil, err
	}
	snapshot, err := d.prepareAdmission(ctx, admitRaw)
	if err != nil {
		return nil, err
	}
	if len(oldEnv.Job.ApprovedScope) > 0 || len(declaredFiles) > 0 || transferred.ThreadID != "" {
		var newEnv envelope
		if decodeEnvelope(snapshot.Payload, &newEnv) == nil {
			if len(p.Scope) > 0 {
				newEnv.Job.ApprovedScope = engine.ScopedFiles(nil, declaredFiles)
			} else if len(oldEnv.Job.ApprovedScope) > 0 {
				newEnv.Job.ApprovedScope = engine.ScopedFiles(newEnv.Job.ApprovedScope, oldEnv.Job.ApprovedScope)
			} else if len(declaredFiles) > 0 {
				newEnv.Job.ApprovedScope = engine.ScopedFiles(newEnv.Job.ApprovedScope, declaredFiles)
			}
			if len(declaredFiles) > 0 {
				newEnv.Job.DeclaredFiles = append([]string(nil), declaredFiles...)
			}
			if transferred.ThreadID != "" {
				newEnv.Job.InterruptedSessionID = transferred.ThreadID
			}
			if payload, err := json.Marshal(newEnv); err == nil {
				snapshot.Payload = payload
			}
		}
	}
	saved, err := d.writeAdmissionRow(ctx, audit, key, snapshot)
	if err != nil {
		return nil, err
	}
	_ = d.store.Mutate(ctx, audit, func(ctx context.Context, tx *sql.Tx) error {
		if _, transferredInDB, err := d.store.TransferTaskSessionTx(ctx, tx, p.Task, newID, now); err != nil {
			return err
		} else if !transferredInDB {
			return d.store.SaveProviderSessionTx(ctx, tx, transferred)
		}
		return nil
	})
	if _, exists := d.jobs[saved.ID]; !exists && !saved.Replayed() {
		d.jobs[saved.ID] = saved
	}
	d.publishStatus()
	d.signal()
	d.notifyEvents()
	return TaskFollowUpResponse{
		ID:                saved.ID,
		FollowUpOf:        p.Task,
		State:             saved.State,
		Version:           saved.Version,
		WorkflowID:        saved.WorkflowID,
		WorkflowRevision:  saved.WorkflowRevision,
		DeclaredFiles:     declaredFiles,
		ReadOnlyFiles:     readOnlyFiles,
		Reads:             reads,
		Budget:            p.Budget,
		ProviderSessionID: transferred.ThreadID,
		ThreadID:          transferred.ThreadID,
	}, nil
}

func (d *Daemon) workflowRevisionHash(doc workflow.Document, rev int64) string {
	if h, err := workflow.Hash(doc); err == nil && h != "" {
		return h
	}
	if rev > 0 {
		return fmt.Sprint(rev)
	}
	return "1"
}

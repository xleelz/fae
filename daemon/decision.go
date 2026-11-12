package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// DecideParams disposes a task's pending HumanDecision (FAE-WF-030). Option
// names one declared answer; Decision optionally pins the pending decision id
// read from the inbox, so a decision is never applied to a later visit.
type DecideParams struct {
	Branch             string                  `json:"branch,omitempty"`
	Items              []workflow.ItemDecision `json:"items,omitempty"`
	ID                 string                  `json:"id"`
	Option             string                  `json:"option"`
	Decision           string                  `json:"decision,omitempty"`
	Rationale          string                  `json:"rationale,omitempty"`
	AuthorizingMessage string                  `json:"authorizing_message"`
}

// decide records the decision artifact citing the authorizing message, the
// decision row, the transition and the audit row in one plane transaction.
func (d *Daemon) decide(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("unresolved checkpoint must reconcile before a decision")
	}
	var p DecideParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.ID == "" || (p.Option == "" && p.Items == nil) {
		return nil, errors.New("decide requires a task and an option")
	}
	s, ok, lookupErr := d.storedJob(ctx, p.ID)
	if lookupErr != nil {
		return nil, lookupErr
	}
	if !ok {
		return nil, errors.New("unknown task")
	}
	var e envelope
	if err := decodeEnvelope(s.Payload, &e); err != nil {
		return nil, err
	}
	if d.active[p.ID] && e.Job.Fan == nil {
		return nil, errors.New("decision refused: a dispatch is in flight")
	}
	before := e.Job
	next, events, rec, err := engine.Decide(e.Job, engine.Decision{ID: p.Decision, Branch: p.Branch, Option: p.Option, Items: p.Items, Message: audit.MessageID, Actor: audit.Actor, Rationale: p.Rationale, At: time.Now().UTC()})
	if err != nil {
		return nil, err
	}
	if before.Fan != nil {
		if err := d.integrateFanBranchesAtJoin(ctx, p.ID, e.Admission.RunID, before.Fan, &next); err != nil {
			return nil, err
		}
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	original := s
	e.Job = next
	if s.Payload, err = json.Marshal(e); err != nil {
		return nil, err
	}
	s.State, s.Events = string(next.State), nil
	for _, event := range events {
		b, _ := json.Marshal(event)
		s.Events = append(s.Events, controlplane.Event{Kind: event.Kind, JobID: s.ID, Payload: b})
	}
	s.Decisions = []controlplane.Decision{{ID: rec.ID, Kind: "HumanDecision", Payload: payload, Rationale: rec.Rationale, Role: rec.Actor, SessionID: rec.ID}}
	saved, err := d.saveOperatorCheckpoint(ctx, audit, original, s)
	if err != nil {
		return nil, err
	}
	d.jobs[p.ID] = saved
	d.applyDecisionCancellations(p.ID, before, next, events)
	d.notifyEvents()
	d.publishStatus()
	d.signal()
	return map[string]any{"id": p.ID, "decision_id": rec.ID, "branch": rec.Branch, "stage": rec.Stage, "option": rec.Option, "next": rec.Next, "artifact_id": rec.ArtifactID, "state": saved.State, "version": saved.Version}, nil
}

// pendingDecision is the inbox view of an undisposed HumanDecision.
func pendingDecision(j engine.Job) *engine.DecisionRequest {
	if req, ok := engine.PendingDecision(j); ok {
		return &req
	}
	return nil
}

func (d *Daemon) applyDecisionCancellations(task string, before, next engine.Job, events []engine.Event) {
	for _, event := range events {
		if event.Kind != "branch-cancelled" || event.DispatchID == "" {
			continue
		}
		if cancel := d.inFlightCancel[event.DispatchID]; cancel != nil {
			cancel()
		}
		d.clearDispatchCancel(task, event.DispatchID)
		for _, dispatch := range engine.BranchDispatches(before) {
			if dispatch.ID == event.DispatchID {
				d.clearBranchActive(task, dispatch.BranchID)
			}
		}
	}
	if !d.hasActiveBranches(task) && next.InFlight == nil {
		delete(d.active, task)
	}
}

package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"sort"
	"time"
)

func (d *Daemon) setupScenario(ctx context.Context) error {
	scripted, ok := d.executor.(*engine.ScenarioScripted)
	if !ok {
		return nil
	}
	if d.live != nil {
		return errors.New("rule scenarios require scripted execution")
	}
	scenario := scripted.Scenario()
	type pin struct {
		id       string
		revision int
		raw      json.RawMessage
		document workflow.Document
	}
	pins := []pin{}
	for key, doc := range scenario.Workflows {
		id, revision, err := workflow.ScenarioCatalogRevision(key, doc)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(doc)
		normalized, err := d.parseWorkflowWrite(raw)
		if err != nil {
			return err
		}
		raw, _ = json.Marshal(normalized)
		pins = append(pins, pin{id: id, revision: revision, raw: raw, document: normalized})
	}
	sort.Slice(pins, func(i, j int) bool {
		if pins[i].id == pins[j].id {
			return pins[i].revision < pins[j].revision
		}
		return pins[i].id < pins[j].id
	})
	for _, p := range pins {
		if _, stored, err := d.store.GetWorkflow(ctx, p.id, int64(p.revision)); err == nil {
			if !samePinnedJSON(p.raw, stored) {
				return fmt.Errorf("scenario workflow %s revision%d differs from stored pin", p.id, p.revision)
			}
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		next := int64(1)
		if latest, _, err := d.store.GetWorkflow(ctx, p.id, 0); err == nil {
			next = latest.Number + 1
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if next != int64(p.revision) {
			return fmt.Errorf("scenario catalog %s revision%d has a gap; provide missing earlier pins", p.id, p.revision)
		}
		f, err := workflow.DeriveFootprint(p.document, func(ref workflow.WorkflowRef) (workflow.Document, error) {
			for _, candidate := range pins {
				if candidate.id == ref.ID && candidate.revision == ref.Revision {
					return candidate.document, nil
				}
			}
			_, raw, e := d.storedWorkflow(ctx, ref.ID, int64(ref.Revision))
			if e != nil {
				return workflow.Document{}, e
			}
			return d.parseWorkflow(raw)
		}, p.document.MaxSessions)
		if err != nil {
			return err
		}
		footprintRaw, _ := json.Marshal(f)
		installed, err := d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "daemon", Action: "ScenarioWorkflowInstall", Outcome: "ok"}, p.id, p.id, p.raw, d.validWorkflowWrite, footprintRaw)
		if err != nil {
			return err
		}
		if installed.Number != int64(p.revision) {
			return fmt.Errorf("scenario catalog installed unexpected revision")
		}
	}

	scripted.SetActionSink(d.scenarioActions)
	return nil
}

// Executor callbacks enter the same writer lock as RPC admission. Literal
// scenario files never gain live provider or external-effect authority.
func (d *Daemon) scenarioActions(ctx context.Context, dispatch engine.Dispatch, rule workflow.ScenarioRule) error {
	if len(rule.Detached) == 0 && rule.ChangeRequest == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return errors.New("scenario action waiting for pending checkpoint")
	}
	old, ok := d.jobs[dispatch.TaskID]
	if !ok {
		return errors.New("scenario parent no longer active")
	}
	var parent envelope
	if err := decodeEnvelope(old.Payload, &parent); err != nil {
		return err
	}
	for _, event := range parent.Job.History {
		if event.Kind == "scenario-actions" && event.DispatchID == dispatch.ID {
			return nil
		}
	}
	events := []engine.Event{}
	for i, a := range rule.Detached {
		// The dispatch and action index make a retry reuse the original child.
		actionTitle := fmt.Sprintf("scenario %s detached action %d", dispatch.ID, i)
		rows, err := d.store.ListJobs(ctx, 100000)
		if err != nil {
			return err
		}
		already := false
		for _, row := range rows {
			var stored envelope
			if decodeEnvelope(row.Payload, &stored) == nil && stored.Admission.ParentTask == dispatch.TaskID && stored.Admission.Title == actionTitle {
				already = true
				break
			}
		}
		if already {
			continue
		}
		childID, err := d.nextDetachedID(ctx, dispatch.TaskID)
		if err != nil {
			return err
		}
		revision := a.Revision
		if revision == 0 {
			revision = 1
		}
		p := AdmitParams{Title: actionTitle, ID: childID, WorkflowID: a.Workflow, Revision: int64(revision), ParentTask: dispatch.TaskID, Inputs: a.Inputs, FromParent: a.FromParent, MaxSessions: a.MaxSessions, Tree: parent.Job.Tree, RunID: parent.Admission.RunID, Priority: parent.Admission.Priority, Mode: "Local", AuthorizingMessage: parent.Admission.AuthorizingMessage}
		raw, _ := json.Marshal(p)
		candidate, err := d.prepareAdmission(ctx, raw)
		if err != nil {
			event := engine.Event{Kind: "scenario-refused", Stage: dispatch.Stage, DispatchID: dispatch.ID, Cause: err.Error()}
			parent.Job.History = append(parent.Job.History, event)
			events = append(events, event)
			continue
		}
		saved, err := d.writeAdmissionRow(ctx, controlplane.Audit{Actor: "daemon", Action: "ScenarioDetachedAdmit", MessageID: p.AuthorizingMessage, Inputs: raw, Outcome: "ok"}, childID, candidate)
		if err != nil {
			return err
		}
		d.jobs[childID] = saved
	}
	if request := rule.ChangeRequest; request != nil {
		target := request.Target
		if target == "parent" && parent.Job.Parent != nil {
			target = parent.Job.Parent.Task
		}
		valid := false
		for _, id := range parent.Job.Ancestors {
			valid = valid || id == target
		}
		if !valid {
			return errors.New("scenario change request target is not an ancestor")
		}
		r := engine.AncestorChangeRequest{ID: dispatch.ID + "/change-request", Source: dispatch.TaskID, Target: target, Artifact: request.Artifact, Value: request.Value, State: "pending", CreatedAt: time.Now().UTC()}
		parent.Job.AncestorRequests = append(parent.Job.AncestorRequests, r)
		event := engine.Event{Kind: "change-request", Stage: dispatch.Stage, DispatchID: dispatch.ID, Request: &r, Cause: "scenario change request"}
		parent.Job.History = append(parent.Job.History, event)
		events = append(events, event)
	}
	marker := engine.Event{Kind: "scenario-actions", Stage: dispatch.Stage, DispatchID: dispatch.ID, Cause: "scripted scenario actions applied"}
	parent.Job.History = append(parent.Job.History, marker)
	events = append(events, marker)
	saved, err := d.persist(ctx, old, parent, events)
	if err != nil {
		return err
	}
	d.jobs[dispatch.TaskID] = saved
	d.notifyEvents()
	d.signal()
	return nil
}

func (d *Daemon) reconcileScenarioDecisions(ctx context.Context) error {
	scripted, ok := d.executor.(*engine.ScenarioScripted)
	if !ok || d.pendingCheckpoint != nil {
		return nil
	}
	ids := []string{}
	for id := range d.jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		old := d.jobs[id]
		var e envelope
		if err := decodeEnvelope(old.Payload, &e); err != nil {
			continue
		}
		for _, request := range engine.PendingDecisions(e.Job) {
			stillPending := false
			for _, current := range engine.PendingDecisions(e.Job) {
				if current.ID == request.ID {
					stillPending = true
				}
			}
			if !stillPending {
				continue
			}
			rule, matched, err := scripted.Match(engine.Dispatch{TaskID: id, Stage: request.Stage, Visit: request.Visit, BranchID: request.Branch}, "")
			if err != nil {
				return err
			}
			if !matched || (rule.Decision == "" && rule.Items == nil) {
				continue
			}
			before := e.Job
			next, events, rec, err := engine.Decide(e.Job, engine.Decision{ID: request.ID, Branch: request.Branch, Option: rule.Decision, Items: rule.Items, Message: e.Admission.AuthorizingMessage, Actor: "operator"})
			if err != nil {
				return taskErr("scenario decision", err)
			}
			payload, _ := json.Marshal(rec)
			old.Decisions = []controlplane.Decision{{ID: rec.ID, Kind: "HumanDecision", Payload: payload, Rationale: rec.Rationale, Role: rec.Actor, SessionID: rec.ID}}
			e.Job = next
			saved, err := d.persist(ctx, old, e, events)
			if err != nil {
				return err
			}
			d.jobs[id] = saved
			d.applyDecisionCancellations(id, before, next, events)
			old = saved
			d.signal()
		}

	}
	return nil
}

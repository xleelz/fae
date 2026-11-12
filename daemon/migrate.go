package daemon

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
)

// MigrateParams moves a task to a named revision of its own workflow
// (FAE-OPS-010 Migrate, FAE-WF-093); Stage optionally names the mapped stage.
type MigrateParams struct {
	ID                 string `json:"id"`
	Revision           int64  `json:"revision"`
	Stage              string `json:"stage,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
}

func (d *Daemon) migrate(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("unresolved checkpoint must reconcile before migration")
	}
	var p MigrateParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Revision < 1 {
		return nil, errors.New("migrate requires a named revision >= 1")
	}
	s, ok, lookupErr := d.storedJob(ctx, p.ID)
	if lookupErr == nil && !ok {
		// A task quarantined for a revision this binary cannot run migrates
		// out of quarantine (FAE-WF-093; quarantine.go).
		s, ok, lookupErr = d.unrunnableQuarantinedRow(ctx, p.ID)
	}
	if lookupErr != nil {
		return nil, lookupErr
	}
	if !ok {
		return nil, errors.New("unknown task")
	}
	if d.active[p.ID] {
		return nil, errors.New("migration refused: a dispatch is in flight; the task finishes on its pinned revision")
	}
	var e envelope
	if err := decodeEnvelope(s.Payload, &e); err != nil {
		return nil, err
	}
	rev, doc, err := d.store.GetWorkflow(ctx, s.WorkflowID, p.Revision)
	if err != nil {
		return nil, errors.New("unknown revision of the task's workflow")
	}
	document, err := d.parseWorkflow(doc)
	if err != nil {
		return nil, err
	}
	tools, err := d.admissionTools(ctx, e.Admission)
	if err != nil {
		return nil, err
	}
	if document, err = normalContent(document, e.Admission.Mode, tools); err != nil {
		return nil, err
	}
	if d.live != nil && d.live.guard.panes != nil {
		if err = validatePaneFloor(document, d.live.guard.effectivePaneLimit()); err != nil {
			return nil, err
		}
	}
	next, events, err := engine.Migrate(e.Job, document, int(rev.Number), p.Stage)
	if err != nil {
		return nil, err
	}
	original := s
	e.Job = next
	if err = d.deriveJobFootprint(ctx, &e.Job); err != nil {
		return nil, err
	}
	if runningSlotState(e.Job.State) && d.workflowSlots != nil {
		decision, e2 := d.ensureWorkflowSlot(ctx, &e)
		if e2 != nil {
			return nil, e2
		}
		if !decision.Granted {
			var waitEvents []engine.Event
			e.Job, waitEvents, e2 = engine.WorkflowSlotWait(e.Job, decision.Reason, decision.Impossible)
			events = append(events, waitEvents...)
			if e2 != nil {
				return nil, e2
			}
		}
	}
	if s.Payload, err = json.Marshal(e); err != nil {
		return nil, err
	}
	s.State, s.WorkflowRevision, s.Events = string(e.Job.State), rev.Number, nil
	for _, event := range events {
		b, _ := json.Marshal(event)
		s.Events = append(s.Events, controlplane.Event{Kind: event.Kind, JobID: s.ID, Payload: b})
	}
	// The same lost-acknowledgement protocol as task controls: only the exact
	// proposed next version may update the cache; the migration is never repeated.
	d.pendingCheckpoint = &jobCheckpoint{Before: original, Proposed: s, Control: true}
	saved, err := d.store.MigrateJob(ctx, audit, s, s.Version, original.WorkflowRevision)
	if err != nil {
		actual, readErr := d.store.GetJob(ctx, s.ID)
		expected := s
		expected.Version++
		if readErr != nil || !sameJobCheckpoint(actual, expected) {
			if readErr == nil && sameJobCheckpoint(actual, original) {
				d.pendingCheckpoint = nil
			}
			d.signal()
			return nil, err
		}
		saved = actual
	}
	d.pendingCheckpoint = nil
	d.logParksAndEscalations(s.Events)
	if err = d.adoptMigrated(ctx, saved); err != nil {
		return nil, err
	}
	d.notifyEvents()
	d.publishStatus()
	d.signal()
	mapped := ""
	if len(events) > 0 {
		mapped = events[0].Stage
	}
	result := map[string]any{"id": p.ID, "from_revision": original.WorkflowRevision, "revision": saved.WorkflowRevision, "state": saved.State, "mapped_stage": mapped, "version": saved.Version}
	var waiting envelope
	if decodeEnvelope(saved.Payload, &waiting) == nil && waiting.Job.SlotWaitReason != "" {
		result["waiting"] = true
		result["withheld_reason"] = waiting.Job.SlotWaitReason
	}
	return result, nil
}

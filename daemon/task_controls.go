package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
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
	s, ok := d.jobs[p.ID]
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
	next, events, err := engine.ApplyControl(e.Job, engine.Control{Method: method, Reason: p.Reason, Information: p.Information, Context: p.Context, Until: p.Until, Priority: p.Priority, AttemptLimit: p.AttemptLimit}, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if d.live != nil && next.State != e.Job.State && (method == "RestartWithInformation" || method == "Resume") {
		if next.ExecutionHead, err = d.live.checkControlWorkspace(ctx, e); err != nil {
			return nil, err
		}
	}
	e.Job = next
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

func (d *Daemon) orderedTaskIDs() ([]string, error) {
	ids := make([]string, 0, len(d.jobs))
	priority := map[string]int{}
	for id, s := range d.jobs {
		var e envelope
		if err := decodeEnvelope(s.Payload, &e); err != nil {
			return nil, err
		}
		ids = append(ids, id)
		priority[id] = e.Admission.Priority
	}
	sort.Slice(ids, func(i, j int) bool {
		if priority[ids[i]] != priority[ids[j]] {
			return priority[ids[i]] > priority[ids[j]]
		}
		return ids[i] < ids[j]
	})
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

func (d *Daemon) saveOperatorCheckpoint(ctx context.Context, audit controlplane.Audit, original, s controlplane.JobSnapshot) (controlplane.JobSnapshot, error) {
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
	original := s
	e.Job = next
	s.Payload, err = json.Marshal(e)
	if err != nil {
		return nil, err
	}
	s.State = string(next.State)
	s.Events = nil
	for _, event := range events {
		raw, _ := json.Marshal(event)
		s.Events = append(s.Events, controlplane.Event{Kind: event.Kind, JobID: s.ID, Payload: raw})
	}
	saved, err := d.saveOperatorCheckpoint(ctx, audit, original, s)
	if err != nil {
		return nil, err
	}
	d.jobs[s.ID] = saved
	d.notifyEvents()
	d.publishStatus()
	d.signal()
	return map[string]any{"id": s.ID, "dispatch_id": p.DispatchID, "action": p.Action, "version": saved.Version}, nil
}

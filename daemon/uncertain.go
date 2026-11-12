package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/workflow"
	"os/exec"
	"strings"
)

type ResolveUncertainParams struct {
	ID                 string         `json:"id"`
	DispatchID         string         `json:"dispatch_id"`
	Action             string         `json:"action"`
	Result             *engine.Result `json:"result,omitempty"`
	AuthorizingMessage string         `json:"authorizing_message"`
}

func (d *Daemon) resolveUncertain(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	var p ResolveUncertainParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Action != "accept" && p.Result != nil {
		return nil, errors.New("result is only valid for accept")
	}
	s, ok, lookupErr := d.storedJob(ctx, p.ID)
	if lookupErr != nil {
		return nil, lookupErr
	}
	if !ok {
		return nil, errors.New("unknown job")
	}
	var env envelope
	if err := decodeEnvelope(s.Payload, &env); err != nil {
		return nil, err
	}
	j := env.Job
	if d.live == nil && j.OperatorStage != "" {
		return d.resolveScriptedOperatorHold(ctx, p, audit, s, env)
	}
	if d.live == nil || j.InFlight == nil || d.active[p.ID] {
		return nil, errors.New("no retired live dispatch")
	}
	// The resolution commits as an operator checkpoint, and one at a time.
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("unresolved checkpoint must reconcile first")
	}
	if !j.Parked || j.Reason != "UncertainDispatch" || j.InFlight.ID != p.DispatchID {
		return nil, errors.New("not the current uncertain dispatch")
	}
	retired := true
	proofs, err := d.live.guard.journal.Entries()
	if err != nil {
		return nil, err
	}
	for _, proof := range proofs {
		for _, session := range j.InFlight.Sessions {
			if proof.Session == session.ID {
				retired = false
			}
		}
	}
	isEffect := supportedEffect(j.InFlight.Config.Primitive)
	var effectResult *engine.Result
	unchanged := true
	if p.Action != "escalate" {
		binding, e := d.live.manager.Ensure(ctx, env.Admission.RunID, j.TaskID, d.live.config.Base)
		if e != nil {
			return nil, e
		}
		w := d.live.bindingPath(binding.Path)
		if isEffect && p.Action == "accept" {
			if p.Result != nil {
				return nil, errors.New("local effect adoption refuses caller-supplied result; use daemon receipt")
			}
			result, e := d.recordedEffect(ctx, *j.InFlight, w, retired, j.Document)
			if e != nil {
				return nil, e
			}
			effectResult = &result
		} else {
			tree, e := d.live.broker.Tree(ctx, w)
			if e != nil {
				return nil, e
			}
			cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
			cmd.Dir = binding.Path
			cmd.Env = cleanGitEnv()
			head, e := cmd.Output()
			if e != nil {
				return nil, e
			}
			unchanged = tree == j.Tree && strings.TrimSpace(string(head)) == j.Base
			if p.Action == "redispatch" && j.InFlight.Config.Primitive != workflow.AgentTurn && j.InFlight.Config.Primitive != workflow.Review && !d.effectNonExecution(ctx, *j.InFlight) && !d.rerunnableEffect(ctx, *j.InFlight) {
				return nil, errors.New("effect may have executed; recorded adoption or escalation required")
			}
		}
	}
	if !retired && (isEffect || j.InFlight.Config.Primitive == workflow.Merge) && (effectResult != nil || (p.Action == "redispatch" && (d.effectNonExecution(ctx, *j.InFlight) || d.rerunnableEffect(ctx, *j.InFlight)))) {
		// Completed receipts and an intent with no started marker are positive
		// trusted retirement/nonexecution evidence even when no CAS was needed.
		if e := d.retireReceiptLeases(ctx, *j.InFlight); e != nil {
			return nil, e
		}
		retired = true
	}
	if !kernel.CanResolveUncertain(audit.MessageID != "", j.Parked && j.Reason == "UncertainDispatch", j.InFlight.ID == p.DispatchID, retired, unchanged) {
		return nil, errors.New("uncertainty resolution requires matching dispatch, retired leases and unchanged tree")
	}
	var next engine.Job
	var events []engine.Event
	if effectResult != nil {
		next, events, err = engine.AdoptEffect(j, p.DispatchID, *effectResult)
	} else if p.Action == "redispatch" && d.effectNonExecution(ctx, *j.InFlight) {
		next, events, err = engine.ResolveUncertainNonExecution(j, p.DispatchID)
	} else {
		next, events, err = engine.ResolveUncertain(j, p.DispatchID, p.Action, p.Result)
	}
	if err != nil {
		return nil, err
	}
	return d.saveUncertainResolution(ctx, audit, p, s, env, next, events)
}

// saveUncertainResolution commits a resolved uncertain dispatch, live or a
// scripted operator hold, as an operator checkpoint: saveOperatorCheckpoint
// validates the row again before saving it, so a resolution never stores a
// row the drive would quarantine (FAE-OPS-010, FAE-ROB-001).
func (d *Daemon) saveUncertainResolution(ctx context.Context, audit controlplane.Audit, p ResolveUncertainParams, original controlplane.JobSnapshot, env envelope, next engine.Job, events []engine.Event) (any, error) {
	s := original
	env.Job = next
	var err error
	s.Payload, err = json.Marshal(env)
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
	d.jobs[s.ID] = saved
	d.notifyEvents()
	d.publishStatus()
	d.signal()
	return map[string]any{"id": s.ID, "dispatch_id": p.DispatchID, "action": p.Action, "version": saved.Version}, nil
}

// rerunnableEffect: an interrupted brokered command or identity measurement is
// infrastructure, re-run on an unchanged tree (FAE-ROB-013). A completed
// receipt must be adopted instead, so its result is never discarded. A
// reconcile (Merge) keeps no receipt; on the unchanged tree and head the
// caller requires, it changed nothing and re-runs (FAE-ROB-012).
func (d *Daemon) rerunnableEffect(ctx context.Context, dispatch engine.Dispatch) bool {
	switch dispatch.Config.Primitive {
	case workflow.HostCommand, workflow.FreezeArtifact, workflow.CompareFrozen, workflow.Merge:
	default:
		return false
	}
	_, err := d.store.GetMarker(ctx, effectKey(dispatch)+"/result")
	return errors.Is(err, sql.ErrNoRows)
}

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/processowner"
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
	s, ok := d.jobs[p.ID]
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
			result, e := d.recordedEffect(ctx, *j.InFlight, w, retired)
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
			if p.Action == "redispatch" && j.InFlight.Config.Primitive != workflow.AgentTurn && j.InFlight.Config.Primitive != workflow.Review && !d.effectNonExecution(ctx, *j.InFlight) {
				return nil, errors.New("effect may have executed; recorded adoption or escalation required")
			}
		}
	}
	if !retired && isEffect && (effectResult != nil || (p.Action == "redispatch" && d.effectNonExecution(ctx, *j.InFlight))) {
		// Completed receipts and an intent with no started marker are positive
		// trusted retirement/nonexecution evidence even when no CAS was needed.
		for _, proof := range proofs {
			for _, session := range j.InFlight.Sessions {
				if proof.Session == session.ID {
					if !processowner.ProvedDead(proof.Owner.PID, proof.Birth) {
						return nil, errors.New("effect lease owner death unproven")
					}
					proof.State = "retired"
					if e := d.live.guard.journal.Put(proof); e != nil {
						return nil, e
					}
				}
			}
		}
		if e := d.live.guard.recoverProofs(ctx); e != nil {
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
	} else {
		next, events, err = engine.ResolveUncertain(j, p.DispatchID, p.Action, p.Result)
	}
	if err != nil {
		return nil, err
	}
	env.Job = next
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
	saved, err := d.store.SaveJob(ctx, audit, s, s.Version)
	if err != nil {
		return nil, err
	}
	d.jobs[p.ID] = saved
	d.notifyEvents()
	d.publishStatus()
	d.signal()
	return map[string]any{"id": p.ID, "dispatch_id": p.DispatchID, "action": p.Action, "version": saved.Version}, nil
}

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
)

type SupplyArtifactParams struct {
	ID                 string          `json:"id"`
	Stage              string          `json:"stage,omitempty"`
	Artifact           json.RawMessage `json:"artifact"`
	AuthorizingMessage string          `json:"authorizing_message"`
}

func (d *Daemon) supplyArtifact(ctx context.Context, raw json.RawMessage, a controlplane.Audit) (any, error) {
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("unresolved checkpoint must reconcile before artifact supply")
	}
	var p SupplyArtifactParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.ID == "" || len(p.Artifact) == 0 {
		return nil, errors.New("artifact supply requires task and artifact")
	}
	if d.active[p.ID] {
		return nil, errors.New("artifact supply refused while task is active")
	}
	old, found, err := d.storedJob(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("unknown task")
	}
	var e envelope
	if err = decodeEnvelope(old.Payload, &e); err != nil {
		return nil, err
	}
	next, events, err := engine.SupplyArtifact(e.Job, p.Stage, p.Artifact, a.MessageID, a.Actor)
	if err != nil {
		return nil, err
	}
	e.Job = next
	candidate := old
	candidate.State = string(next.State)
	candidate.Payload, err = json.Marshal(e)
	if err != nil {
		return nil, err
	}
	candidate.Events = nil
	for _, event := range events {
		b, _ := json.Marshal(event)
		candidate.Events = append(candidate.Events, controlplane.Event{Kind: event.Kind, JobID: p.ID, Payload: b})
	}
	saved, err := d.saveOperatorCheckpoint(ctx, a, old, candidate)
	if err != nil {
		return nil, err
	}
	d.jobs[p.ID] = saved
	d.publishStatus()
	d.notifyEvents()
	d.signal()
	return map[string]any{"id": p.ID, "stage": p.Stage, "state": saved.State, "version": saved.Version}, nil
}

package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/loops"
)

// A failed marker acknowledgement must retain the exact proposal. Cache state,
// worker ownership and dispatch counters change only after durable confirmation.
// The original drive operation consumes the proposal after rearming; this keeps
// a collected envelope and a not-yet-launched dispatch distinct.
type loopCheckpoint struct {
	Kind, ID         string
	Before, Proposed json.RawMessage
	Audit            controlplane.Audit
	Acknowledged     bool
}

func (d *Daemon) prepareLoopCheckpoint(ctx context.Context, kind, id string, s loops.Schedule, a controlplane.Audit) error {
	if d.loopCheckpoint != nil {
		return errors.New("loop checkpoint already pending")
	}
	before, e := json.Marshal(d.loops)
	if e != nil {
		return e
	}
	next := make(map[string]loops.Schedule, len(d.loops))
	for k, v := range d.loops {
		next[k] = v
	}
	next[id] = s
	proposed, e := json.Marshal(next)
	if e != nil {
		return e
	}
	d.loopCheckpoint = &loopCheckpoint{Kind: kind, ID: id, Before: before, Proposed: proposed, Audit: a}
	e = d.store.PutMarker(ctx, a, "provider-loops", proposed)
	if e == nil {
		d.loopCheckpoint.Acknowledged = true
		return nil
	}
	// The next generation can retry the exact proposal even if this read fails.
	if recovered := d.reconcileLoopCheckpoint(ctx); recovered != nil {
		return errors.Join(e, recovered)
	}
	return nil
}

// Called under d.mu while the drive is stopped. It does not publish cache state
// or forget a worker result; collection/dispatch will consume the saved proposal.
func (d *Daemon) reconcileLoopCheckpoint(ctx context.Context) error {
	p := d.loopCheckpoint
	if p == nil || p.Acknowledged {
		return nil
	}
	raw, e := d.store.GetMarker(ctx, "provider-loops")
	if errors.Is(e, sql.ErrNoRows) {
		raw = json.RawMessage(`{}`)
	} else if e != nil {
		return e
	}
	actual, e := controlplane.CanonicalJSON(raw)
	if e != nil {
		return e
	}
	proposed, _ := controlplane.CanonicalJSON(p.Proposed)
	before, _ := controlplane.CanonicalJSON(p.Before)
	if bytes.Equal(actual, proposed) {
		p.Acknowledged = true
		return nil
	}
	if !bytes.Equal(actual, before) {
		return errors.New("loop checkpoint diverged")
	}
	if e = d.store.PutMarker(ctx, p.Audit, "provider-loops", p.Proposed); e != nil {
		return e
	}
	p.Acknowledged = true
	return nil
}
func (d *Daemon) consumeLoopCheckpoint(ctx context.Context, kind, id string) (loops.Schedule, error) {
	var zero loops.Schedule
	p := d.loopCheckpoint
	if p == nil || p.Kind != kind || p.ID != id {
		return zero, errors.New("different loop checkpoint pending")
	}
	if e := d.reconcileLoopCheckpoint(ctx); e != nil {
		return zero, e
	}
	var next map[string]loops.Schedule
	if e := json.Unmarshal(p.Proposed, &next); e != nil {
		return zero, e
	}
	d.loops = next
	d.loopCheckpoint = nil
	return next[id], nil
}

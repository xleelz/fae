package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xleelz/fae/engine"
)

// Called by the serialized drive after refreshing dynamic capacity. Inventory
// failure never authorizes shedding; the normal launch admission independently
// withholds a permit while the census is unreadable.
func (d *Daemon) maintainIdleSessions(ctx context.Context) error {
	if d.idleSessions == nil || d.idleSessions.Count() == 0 || d.live == nil || d.live.guard.inventory == nil {
		return nil
	}
	counts, err := d.live.guard.inventory(ctx)
	if err != nil {
		return nil
	}
	inflight := len(d.active) + len(d.loopActive) + len(d.prophetActive)
	for id, schedule := range d.loops {
		if d.loopActive[id] == nil && schedule.InFlight {
			inflight++
		}
	}
	for id, snapshot := range d.jobs {
		if d.active[id] {
			continue
		}
		var e envelope
		if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
			return err
		}
		if e.Job.InFlight != nil {
			inflight++
		}
	}
	actions, err := d.idleSessions.Round(ctx, counts, d.live.guard.effectivePaneLimit(), inflight, time.Now().UTC())
	if err != nil {
		raw, _ := json.Marshal(map[string]string{"reason": err.Error()})
		return d.store.RecordIdleAction(ctx, "idle-maintenance-refused", raw)
	}
	for _, action := range actions {
		raw, _ := json.Marshal(action)
		if err := d.store.RecordIdleAction(ctx, action.Kind, raw); err != nil {
			return err
		}
		if action.Kind != "PaneCapStarvation" {
			continue
		}
		for id, snapshot := range d.jobs {
			var e envelope
			if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
				return err
			}
			if d.active[id] || !engine.PaneStarved(e.Job) {
				continue
			}
			next, events, err := engine.EscalatePaneStarvation(e.Job)
			if err != nil {
				return err
			}
			e.Job = next
			saved, err := d.persist(ctx, snapshot, e, events)
			if err != nil {
				return err
			}
			d.jobs[id] = saved
		}
	}
	if len(actions) > 0 {
		d.publishStatus()
		d.notifyEvents()
	}
	return nil
}

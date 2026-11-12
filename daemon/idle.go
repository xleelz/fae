package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	agentruntime "github.com/xleelz/fae/runtime"
)

// idleRefusalInterval bounds how often one persisting idle-maintenance
// refusal is recorded: when it starts or its cause changes, then at most once
// per interval, counting the refused rounds left unrecorded since. A change of
// cause also records the previous cause's unrecorded count.
const idleRefusalInterval = 10 * time.Minute

// idleRefusal is the current idle-maintenance refusal episode.
type idleRefusal struct {
	cause      string
	at         time.Time
	suppressed int
}

// Called by the serialized drive, with the writer lock held, after refreshing
// dynamic capacity. Inventory failure never authorizes shedding; the normal
// launch admission independently withholds a permit while the census is
// unreadable. The Herdr census, any session retirement and their audit rows
// run without the writer lock (FAE-ROB-021): the idle registry has its own
// lock. The in-flight count is read under the writer lock after the census,
// where the round uses it, so a turn started during the census is counted;
// the escalation of starved tasks retakes the lock, re-reads the tasks and
// skips both its row and the escalation once anything is in flight. A skipped
// round (an unreadable census, or a refusal by the registry) is a refusal: one
// row, event and line per episode (FAE-OBS-006), never silent.
func (d *Daemon) maintainIdleSessions(ctx context.Context) error {
	if d.idleSessions == nil || d.idleSessions.Count() == 0 || d.live == nil || d.live.guard.inventory == nil {
		return nil
	}
	inventory, limit := d.live.guard.inventory, d.live.guard.effectivePaneLimit()
	var actions []agentruntime.IdleAction
	var err error
	d.mu.Unlock()
	func() {
		defer d.mu.Lock()
		counts, e := inventory(ctx)
		if e != nil {
			// Never shedding, reconciling or retrying a release on an
			// unreadable census (the dead-server policy is the census's own).
			err = d.idleRefused(ctx, "census", "pane census unreadable: "+e.Error())
			return
		}
		d.mu.Lock()
		inflight := d.idleInflight()
		d.mu.Unlock()
		actions, e = d.idleSessions.Round(ctx, counts, limit, inflight, time.Now().UTC())
		for _, action := range actions {
			if action.Kind == "PaneCapStarvation" {
				continue // recorded with its escalation, under the writer lock
			}
			raw, _ := json.Marshal(action)
			if err = d.store.RecordIdleAction(ctx, action.Kind, raw); err != nil {
				return
			}
		}
		if e != nil {
			// A retirement left unproven is retried by the next round; the
			// actions the round completed are recorded above.
			err = d.idleRefused(ctx, "round", e.Error())
			return
		}
		d.idleResumed()
	}()
	if err != nil {
		return err
	}
	for _, action := range actions {
		if action.Kind != "PaneCapStarvation" || d.idleInflight() != 0 {
			continue
		}
		raw, _ := json.Marshal(action)
		if err := d.store.RecordIdleAction(ctx, action.Kind, raw); err != nil {
			return err
		}
		for id, snapshot := range d.jobs {
			var e envelope
			if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
				continue
			}
			if d.active[id] || !engine.PaneStarved(e.Job) {
				continue
			}
			next, events, err := engine.EscalatePaneStarvation(e.Job)
			if err == nil {
				e.Job = next
				var saved controlplane.JobSnapshot
				if saved, err = d.persist(ctx, snapshot, e, events); err == nil {
					d.jobs[id] = saved
				}
			}
			if err != nil {
				if err = d.taskFault(ctx, id, taskErr("idle", err)); err != nil {
					return err
				}
			}
		}
	}
	if len(actions) > 0 {
		d.publishStatus()
		d.notifyEvents()
	}
	return nil
}

// idleRefused records a skipped idle-maintenance round. The row and event
// commit before the line, so neither exists without the other; a refusal of
// the episode's cause within idleRefusalInterval is counted, not recorded.
// A refusal with a new cause ends the episode, and its row and line carry the
// previous cause's unrecorded count, which is never dropped (FAE-OBS-006).
func (d *Daemon) idleRefused(ctx context.Context, cause, reason string) error {
	if ctx.Err() != nil {
		return nil // stopping: the round was cancelled, not refused
	}
	now := time.Now()
	d.obs.mu.Lock()
	episode := d.obs.idleRefusal
	if episode.cause == cause && now.Sub(episode.at) < idleRefusalInterval {
		d.obs.idleRefusal.suppressed++
		d.obs.mu.Unlock()
		return nil
	}
	d.obs.mu.Unlock()
	suppressed := 0
	fields := map[string]any{"cause": cause, "reason": reason}
	previous := ""
	if episode.cause == cause {
		suppressed = episode.suppressed
	} else if episode.cause != "" {
		fields["previous_cause"], fields["previous_unrecorded_refusals"] = episode.cause, episode.suppressed
		previous = fmt.Sprintf(" previous_cause=%s previous_unrecorded_refusals=%d", episode.cause, episode.suppressed)
	}
	fields["unrecorded_refusals"] = suppressed
	raw, _ := json.Marshal(fields)
	if err := d.store.RecordIdleAction(ctx, "idle-maintenance-refused", raw); err != nil {
		return err
	}
	d.obs.mu.Lock()
	d.obs.idleRefusal = idleRefusal{cause: cause, at: now}
	d.obs.mu.Unlock()
	d.notifyEvents()
	d.logLine("idle", "idle maintenance refused cause=%s reason=%q unrecorded_refusals=%d%s\n", cause, reason, suppressed, previous)
	return nil
}

// idleResumed ends a refusal episode once a round completes.
func (d *Daemon) idleResumed() {
	d.obs.mu.Lock()
	episode := d.obs.idleRefusal
	d.obs.idleRefusal = idleRefusal{}
	d.obs.mu.Unlock()
	if episode.cause != "" {
		d.logLine("idle", "idle maintenance resumed after cause=%s unrecorded_refusals=%d\n", episode.cause, episode.suppressed)
	}
}

// idleInflight counts, under d.mu, the turns in flight or pending dispatch.
func (d *Daemon) idleInflight() int {
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
			continue
		}
		if e.Job.InFlight != nil {
			inflight++
		}
	}
	return inflight
}

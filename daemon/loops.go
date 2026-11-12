package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/cron"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/loops"
	"sort"
	"time"
)

type ScheduleParams struct {
	Cron               *cron.Spec  `json:"cron,omitempty"`
	Action             string      `json:"action"`
	ID                 string      `json:"id,omitempty"`
	Spec               *loops.Spec `json:"spec,omitempty"`
	AuthorizingMessage string      `json:"authorizing_message,omitempty"`
	// ExpiresInSeconds sets a loop's expiry from the daemon clock at creation
	// when the specification states no explicit expires_at; with neither, the
	// daemon clock plus the default expiry applies (FAE-CLI-002).
	ExpiresInSeconds int64 `json:"expires_in_seconds,omitempty"`
}
type loopCompletion struct {
	id     string
	result loops.Result
	err    error
}

func (d *Daemon) saveLoops(ctx context.Context, a controlplane.Audit) error {
	if d.loopCheckpoint != nil {
		return errors.New("loop checkpoint pending recovery")
	}
	raw, e := json.Marshal(d.loops)
	if e != nil {
		return e
	}
	return d.store.PutMarker(ctx, a, "provider-loops", raw)
}
func (d *Daemon) loadLoops(ctx context.Context) error {
	d.loops = map[string]loops.Schedule{}
	d.loopActive = map[string]context.CancelFunc{}
	d.loopResults = make(chan loopCompletion, completionBuffer(d.config.MaxInFlight))
	raw, e := d.store.GetMarker(ctx, "provider-loops")
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if e = json.Unmarshal(raw, &d.loops); e != nil {
		return e
	}
	for id, s := range d.loops {
		if s.Spec.ID != id || s.Validate() != nil {
			return errors.New("invalid durable loop identity or expiry")
		}
		s.Recover()
		d.loops[id] = s
	}
	return d.saveLoops(ctx, controlplane.Audit{Actor: "daemon", Action: "loop recovery", Outcome: "ok"})
}
func (d *Daemon) schedule(ctx context.Context, p ScheduleParams, a controlplane.Audit) (any, error) {
	if d.loopCheckpoint != nil && p.Action != "inspect" && p.Action != "list" {
		return nil, errors.New("loop checkpoint pending recovery")
	}
	if p.Action != "create" && (p.Cron != nil || p.Spec != nil) {
		return nil, errors.New("specifications only accepted for create")
	}
	if p.Cron != nil {
		return d.cronSchedule(ctx, p, a)
	}
	if _, ok := d.crons[p.ID]; ok && p.Action != "create" {
		return d.cronSchedule(ctx, p, a)
	}
	if p.Action == "list" {
		ids := make([]string, 0, len(d.loops))
		for id := range d.loops {
			ids = append(ids, id)
		}
		for id := range d.crons {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		rows := make([]any, 0, len(ids))
		for _, id := range ids {
			if s, ok := d.crons[id]; ok {
				rows = append(rows, s)
			} else {
				rows = append(rows, d.loops[id])
			}
		}
		return rows, nil
	}
	if p.Action == "create" {
		if d.draining {
			return nil, errors.New("draining: schedule creation paused")
		}
		if len(d.loops)+len(d.crons) >= 1000 {
			return nil, errors.New("schedule limit reached")
		}
		if p.Spec == nil {
			return nil, errors.New("specification required")
		}
		if _, ok := d.crons[p.Spec.ID]; ok {
			return nil, errors.New("schedule already exists")
		}
		now := time.Now().UTC()
		if p.ExpiresInSeconds != 0 && !p.Spec.ExpiresAt.IsZero() || p.ExpiresInSeconds < 0 {
			return nil, errors.New("a loop states at most one of an explicit expires_at or a positive expires_in_seconds")
		}
		if p.ExpiresInSeconds > 0 {
			p.Spec.ExpiresAt = now.Add(time.Duration(p.ExpiresInSeconds) * time.Second)
		}
		s, e := loops.New(*p.Spec, now)
		if e != nil {
			return nil, e
		}
		if _, ok := d.loops[s.Spec.ID]; ok {
			return nil, errors.New("schedule already exists")
		}
		if d.live != nil && !d.live.config.PersistentLoops {
			return nil, errors.New("live loops require persistent_loops trusted adapter contract")
		}
		if d.live != nil && (s.Spec.Binding.Provider == "" || s.Spec.Binding.Model == "") {
			return nil, errors.New("live loop requires explicit provider/model binding")
		}
		if e = d.prepareLoopCheckpoint(ctx, "admin", s.Spec.ID, s, a); e != nil {
			return nil, e
		}
		s, e = d.consumeLoopCheckpoint(ctx, "admin", s.Spec.ID)
		if e != nil {
			return nil, e
		}
		d.signal()
		return s, nil
	}
	s, ok := d.loops[p.ID]
	if !ok {
		return nil, errors.New("schedule not found")
	}
	if p.Action == "inspect" {
		return s, nil
	}
	switch p.Action {
	case "pause":
		if s.State != "active" {
			return nil, errors.New("only active loops can pause")
		}
		s.State = "paused"
	case "resume":
		if s.State != "paused" || s.InFlight {
			return nil, errors.New("only paused idle loops can resume; uncertain iterations cannot replay")
		}
		s.State = "active"
		s.Bound(time.Now().UTC())
	case "retire":
		s.State = "retired"
		if d.loopActive[p.ID] == nil {
			s.InFlight = false
		}
	default:
		return nil, errors.New("unsupported schedule action")
	}
	if e := d.prepareLoopCheckpoint(ctx, "admin", p.ID, s, a); e != nil {
		return nil, e
	}
	var e error
	s, e = d.consumeLoopCheckpoint(ctx, "admin", p.ID)
	if e != nil {
		return nil, e
	}
	if p.Action == "retire" {
		if cancel := d.loopActive[p.ID]; cancel != nil {
			cancel()
		}
	}
	d.signal()
	return s, nil
}
func (d *Daemon) dispatchLoops() error {
	defer d.publishStatus()
	if p := d.loopCheckpoint; p != nil {
		if p.Kind == "collect" {
			return errors.New("loop collection pending recovery")
		}
		s, e := d.consumeLoopCheckpoint(d.ctx, p.Kind, p.ID)
		if e != nil {
			return e
		}
		if p.Kind == "dispatch" {
			d.launchLoop(p.ID, s)
			return nil
		}
		if p.Kind == "admin" && s.State == "retired" {
			if cancel := d.loopActive[p.ID]; cancel != nil {
				cancel()
			}
		}
	}
	// Alternate contested rounds so self-paced loops cannot consume every newly
	// available slot before the workflow scheduler sees it.
	for _, snapshot := range d.jobs {
		if !domain.State(snapshot.State).Terminal() && snapshot.State != "Escalated" {
			d.loopPriority = !d.loopPriority
			if !d.loopPriority {
				return nil
			}
			break
		}
	}
	ids := make([]string, 0, len(d.loops))
	for id := range d.loops {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := d.loops[ids[i]], d.loops[ids[j]]
		if a.Next.Equal(b.Next) {
			return ids[i] < ids[j]
		}
		return a.Next.Before(b.Next)
	})
	for _, id := range ids {
		s := d.loops[id]
		prior := s.State
		s.Bound(time.Now().UTC())
		if prior != s.State {
			if e := d.prepareLoopCheckpoint(d.ctx, "bound", id, s, controlplane.Audit{Actor: "daemon", Action: "loop bound", Outcome: s.State}); e != nil {
				return e
			}
			var e error
			s, e = d.consumeLoopCheckpoint(d.ctx, "bound", id)
			if e != nil {
				return e
			}
		}
		if len(d.active)+len(d.loopActive) >= d.effectiveTasksInFlight() {
			return nil
		}
		if !s.Due(time.Now().UTC()) {
			continue
		}
		s.InFlight = true
		s.Dispatches++
		raw, _ := json.Marshal(map[string]any{"schedule": id, "session": s.SessionID, "iteration": s.Iterations + 1, "attempt": s.Dispatches})
		if e := d.prepareLoopCheckpoint(d.ctx, "dispatch", id, s, controlplane.Audit{Actor: "daemon", Action: "loop iteration started", Inputs: raw, Outcome: "checkpointed"}); e != nil {
			return e
		}
		var e error
		s, e = d.consumeLoopCheckpoint(d.ctx, "dispatch", id)
		if e != nil {
			return e
		}
		d.launchLoop(id, s)
	}
	return nil
}
func (d *Daemon) launchLoop(id string, s loops.Schedule) {
	ctx, cancel := context.WithDeadline(d.ctx, s.Spec.ExpiresAt)
	d.loopActive[id] = cancel
	request := loops.Request{Attempt: s.Dispatches, Spec: s.Spec, SessionID: s.SessionID, Iteration: s.Iterations + 1, Context: append(json.RawMessage(nil), s.Context...)}
	runner := d.config.LoopRunner
	if runner == nil {
		runner = loops.Scripted
		if d.live != nil {
			runner = d.runLiveLoop
		}
	}
	d.workers.Add(1)
	go func(id string) {
		defer d.workers.Done()
		defer cancel()
		result, e := runner(ctx, request)
		select {
		case d.loopResults <- loopCompletion{id, result, e}:
		case <-d.ctx.Done():
		}
	}(id)
}

// errStaleLoop marks a completion for no in-flight loop: it is dropped.
var errStaleLoop = errors.New("unexpected loop completion")

func (d *Daemon) collectLoop(c loopCompletion) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.publishStatus()
	if d.testFault != nil {
		if e := d.testFault("step", "loop-collect"); e != nil {
			return e
		}
	}
	if p := d.loopCheckpoint; p != nil {
		if p.Kind == "collect" {
			if _, e := d.consumeLoopCheckpoint(d.ctx, "collect", c.id); e != nil {
				return e
			}
			delete(d.loopActive, c.id)
			return nil
		}
		s, e := d.consumeLoopCheckpoint(d.ctx, p.Kind, p.ID)
		if e != nil {
			return e
		}
		if p.Kind == "dispatch" {
			d.launchLoop(p.ID, s)
		}
		if p.Kind == "admin" && s.State == "retired" {
			if cancel := d.loopActive[p.ID]; cancel != nil {
				cancel()
			}
		}
	}
	s, ok := d.loops[c.id]
	if !ok || !s.InFlight {
		return fmt.Errorf("%w %s", errStaleLoop, c.id)
	}
	state := s.State
	s.Complete(c.result, c.err, time.Now().UTC())
	if state == "retired" {
		s.State = "retired"
	}
	raw, _ := json.Marshal(map[string]any{"schedule": c.id, "session": s.SessionID, "iterations": s.Iterations, "error": s.Error})
	action := "loop iteration completed"
	if _, deferred := c.err.(*loops.DeferredError); deferred {
		action = "loop iteration deferred"
	}
	if e := d.prepareLoopCheckpoint(d.ctx, "collect", c.id, s, controlplane.Audit{Actor: "daemon", Action: action, Inputs: raw, Outcome: s.State}); e != nil {
		return e
	}
	if _, e := d.consumeLoopCheckpoint(d.ctx, "collect", c.id); e != nil {
		return e
	}
	delete(d.loopActive, c.id)
	return nil
}

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
	"sync"
	"time"
)

type treeWaiter struct {
	root, id string
	priority int
	admitted time.Time
	order    uint64
}
type treeSessionGate struct {
	mu      sync.Mutex
	wake    chan struct{}
	holders map[string]string
	waiters []treeWaiter
	limits  map[string]int
	serial  uint64
}

func (g *treeSessionGate) init() {
	if g.wake == nil {
		g.wake = make(chan struct{})
		g.holders = map[string]string{}
		g.limits = map[string]int{}
	}
}
func (g *treeSessionGate) signal() { close(g.wake); g.wake = make(chan struct{}) }
func (g *treeSessionGate) set(root string, limit int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	g.limits[root] = limit
	g.signal()
}
func (g *treeSessionGate) acquire(ctx context.Context, root, id string, priority int, admitted time.Time, limit int) (func(), error) {
	g.mu.Lock()
	g.init()
	if _, ok := g.limits[root]; !ok {
		g.limits[root] = limit
	}
	g.serial++
	g.waiters = append(g.waiters, treeWaiter{root, id, priority, admitted, g.serial})
	sort.SliceStable(g.waiters, func(i, j int) bool {
		a, b := g.waiters[i], g.waiters[j]
		if a.priority != b.priority {
			return a.priority > b.priority
		}
		if !a.admitted.Equal(b.admitted) {
			return a.admitted.Before(b.admitted)
		}
		return a.order < b.order
	})
	remove := func() {
		for i, w := range g.waiters {
			if w.id == id {
				g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
				break
			}
		}
	}
	for {
		used := 0
		for _, r := range g.holders {
			if r == root {
				used++
			}
		}
		first := ""
		for _, w := range g.waiters {
			if w.root == root {
				first = w.id
				break
			}
		}
		cap := g.limits[root]
		if first == id && (cap == -1 || used < cap) {
			remove()
			g.holders[id] = root
			g.signal()
			g.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { g.mu.Lock(); delete(g.holders, id); g.signal(); g.mu.Unlock() }) }, nil
		}
		wake := g.wake
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			remove()
			g.signal()
			g.mu.Unlock()
			return nil, ctx.Err()
		case <-wake:
			g.mu.Lock()
		}
	}
}

type TreeLimitParams struct {
	Task               string `json:"task"`
	MaxSessions        int    `json:"max_sessions"`
	AuthorizingMessage string `json:"authorizing_message"`
}

func (d *Daemon) treeLimit(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	var p TreeLimitParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.MaxSessions != -1 && p.MaxSessions < 1 {
		return nil, errors.New("max_sessions must be -1 or positive")
	}
	row, found, err := d.storedJob(ctx, p.Task)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("tree task not found")
	}
	var e envelope
	if err = decodeEnvelope(row.Payload, &e); err != nil {
		return nil, err
	}
	root := engine.RootTask(e.Job)
	target, found, err := d.storedJob(ctx, root)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("tree root not found")
	}
	var rootEnv envelope
	if err = decodeEnvelope(target.Payload, &rootEnv); err != nil {
		return nil, err
	}
	rootEnv.Job.MaxSessions = p.MaxSessions
	if err = d.deriveJobFootprint(ctx, &rootEnv.Job); err != nil {
		return nil, err
	}
	if d.workflowSlots != nil {
		spec, e := d.slotSpec(ctx, rootEnv)
		if e != nil {
			return nil, e
		}
		_, held := d.workflowSlots.held[spec.ID]
		if held || runningSlotState(rootEnv.Job.State) {
			decision, e := d.ensureWorkflowSlot(ctx, &rootEnv)
			if e != nil {
				return nil, e
			}
			if !decision.Granted {
				_ = d.workflowSlots.ledger.CancelWorkflowWaiter(ctx, spec.ID, d.workflowSlots.owner)
				delete(d.workflowSlots.wait, spec.ID)
				return nil, fmt.Errorf("tree session limit refused: %s", decision.Reason)
			}
		}
	}

	body, err := json.Marshal(rootEnv)
	if err != nil {
		return nil, err
	}
	target.Payload = body
	event, _ := json.Marshal(map[string]any{"tree_root": root, "max_sessions": p.MaxSessions})
	target.Events = []controlplane.Event{{Kind: "tree-limit-changed", Payload: event}}
	updated, err := d.store.SaveJob(ctx, audit, target, target.Version)
	if err != nil {
		return nil, err
	}
	if _, ok := d.jobs[root]; ok {
		d.jobs[root] = updated
	}
	if d.live != nil && d.live.guard != nil {
		d.live.guard.treeSessions.set(root, p.MaxSessions)
	}
	return map[string]any{"tree_root": root, "max_sessions": p.MaxSessions}, nil
}
func (a *liveAdmission) acquireTree(ctx context.Context, d engine.Dispatch, s engine.Session) (func(), error) {
	if !agentSession(d.Config.Primitive) && d.Config.Primitive != workflow.HostCommand {
		return func() {}, nil
	}
	v := a.schedule()
	root := v.treeRoot[d.TaskID]
	if root == "" {
		root = d.TaskID
	}
	limit := v.treeLimit[root]
	if limit == 0 && a.daemon != nil && a.daemon.store != nil {
		row, err := a.daemon.store.GetJob(ctx, root)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		var env envelope
		if err == nil {
			if err = decodeEnvelope(row.Payload, &env); err != nil {
				return nil, err
			}
			limit = env.Job.MaxSessions
		}

	}
	if limit == 0 {
		limit = -1
	}
	return a.treeSessions.acquire(ctx, root, a.proofID(s.ID), s.Priority, s.Admitted, limit)
}

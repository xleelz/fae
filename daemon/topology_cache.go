package daemon

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
)

// topologyCache serves the Topology read from a snapshot (FAE-API-001,
// FAE-ROB-020): every running molecule with its stage graph and the
// non-terminal tasks at each stage, and in a separate on-demand slot the
// running molecules with their Retired tasks too (FAE-OPS-011). A keeper
// builds each slot off the request path on demand, as the status keeper does:
// a read that finds its snapshot older than statusRefreshAge wakes the
// keeper, and a read that finds none, or one older than snapshotStaleness,
// waits for the refresh within its deadline. Concurrent reads share one
// refresh, a refresh is one paged plane read, the default refresh never reads
// or counts Retired tasks, and an idle daemon nobody reads does no topology
// work.
type topologyCache struct {
	mu          sync.Mutex
	snapshot    *topologySnapshot
	err         error // the last default refresh's failure, until one succeeds
	withRetired *topologySnapshot
	retiredErr  error // the last include_retired refresh's failure, until one succeeds
	wantLive    bool
	wantRetired bool
	wake, done  chan struct{}
	// refreshes counts the plane refreshes finished (a test seam).
	refreshes atomic.Int64
}

type topologySnapshot struct {
	entries          []topologyEntry // in molecule id order
	at               time.Time
	statements, rows int
	elapsed          time.Duration
}

// topologyEntry is one running molecule's page entry, or the error a page
// that includes it answers: a molecule past the task bound. A molecule whose
// graph does not resolve is an entry with its Error set, so it never hides
// the others.
type topologyEntry struct {
	molecule TopologyMolecule
	err      error
}

// cachedTopology is the topology snapshot (or the include_retired snapshot),
// refreshed first as described above.
func (d *Daemon) cachedTopology(ctx context.Context, includeRetired bool) (*topologySnapshot, error) {
	c := &d.topologyCache
	c.mu.Lock()
	snap := c.snapshot
	if includeRetired {
		snap = c.withRetired
	}
	c.mu.Unlock()
	if snap != nil && time.Since(snap.at) < statusRefreshAge {
		return snap, nil
	}
	done, err := d.requestTopology(includeRetired)
	if err != nil {
		return nil, err
	}
	if snap != nil && time.Since(snap.at) <= snapshotStaleness {
		return snap, nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	c.mu.Lock()
	snap, err = c.snapshot, c.err
	if includeRetired {
		snap, err = c.withRetired, c.retiredErr
	}
	c.mu.Unlock()
	if snap != nil && time.Since(snap.at) <= snapshotStaleness {
		return snap, nil
	}
	if err == nil {
		err = &api.RPCError{Code: "snapshot-stale", Message: "topology snapshot exceeds 10 seconds"}
	}
	return nil, err
}

// requestTopology wakes the keeper, starting it on first use, and returns the
// channel closed once a refresh started after this request has finished.
func (d *Daemon) requestTopology(includeRetired bool) (<-chan struct{}, error) {
	c := &d.topologyCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if d.ctx.Err() != nil {
		return nil, shuttingDownError()
	}
	if c.wake == nil {
		if err := d.beginWorker(); err != nil {
			return nil, err
		}
		c.wake, c.done = make(chan struct{}, 1), make(chan struct{})
		go d.maintainTopology(c.wake)
	}
	if includeRetired {
		c.wantRetired = true
	} else {
		c.wantLive = true
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return c.done, nil
}

// maintainTopology is the keeper: it blocks until a read wakes it, then
// refreshes each requested snapshot slot unless a refresh since has left it
// fresh.
func (d *Daemon) maintainTopology(wake chan struct{}) {
	defer d.workers.Done()
	c := &d.topologyCache
	for {
		select {
		case <-d.ctx.Done():
			c.mu.Lock()
			close(c.done)
			c.done = make(chan struct{})
			c.err, c.retiredErr = d.ctx.Err(), d.ctx.Err()
			c.mu.Unlock()
			return
		case <-wake:
		}
		c.mu.Lock()
		select {
		case <-wake:
		default:
		}
		done := c.done
		c.done = make(chan struct{})
		needLive := c.wantLive && (c.snapshot == nil || time.Since(c.snapshot.at) >= statusRefreshAge)
		needRetired := c.wantRetired && (c.withRetired == nil || time.Since(c.withRetired.at) >= statusRefreshAge)
		c.wantLive, c.wantRetired = false, false
		c.mu.Unlock()
		if needLive {
			snap, err := d.buildTopology(d.ctx, false)
			c.mu.Lock()
			if c.err = err; err == nil {
				c.snapshot = snap
			}
			c.mu.Unlock()
			c.refreshes.Add(1)
		}
		if needRetired {
			snap, err := d.buildTopology(d.ctx, true)
			c.mu.Lock()
			if c.retiredErr = err; err == nil {
				c.withRetired = snap
			}
			c.mu.Unlock()
			c.refreshes.Add(1)
		}
		close(done)
	}
}

// buildTopology pages every running molecule through the scoped Topology read
// (FAE-CP-009) and resolves each stage graph. A page past the task bound is
// re-read narrower; a single molecule past it is kept as its error.
func (d *Daemon) buildTopology(ctx context.Context, includeRetired bool) (*topologySnapshot, error) {
	started := time.Now()
	snap := &topologySnapshot{entries: []topologyEntry{}}
	const pageLimit = 100
	limit := pageLimit
	for cursor := ""; ; {
		read, cancel := context.WithTimeout(ctx, 5*time.Second)
		page, err := d.store.Topology(read, "", cursor, limit, 10000, includeRetired)
		snap.statements, snap.rows = snap.statements+page.Statements, snap.rows+page.Rows
		switch {
		case errors.Is(err, controlplane.ErrTopologyBound) && limit > 1:
			limit /= 2
			cancel()
			continue
		case errors.Is(err, controlplane.ErrTopologyBound) && len(page.Molecules) == 1:
			snap.entries = append(snap.entries, topologyEntry{molecule: TopologyMolecule{Molecule: page.Molecules[0].Molecule}, err: err})
			cursor, limit = page.Molecules[0].ID, pageLimit
			cancel()
			continue
		case err != nil:
			cancel()
			return nil, err
		}
		for _, m := range page.Molecules {
			snap.entries = append(snap.entries, d.topologyMolecule(read, m))
		}
		cancel()
		if page.NextCursor == "" {
			break
		}
		cursor, limit = page.NextCursor, pageLimit
	}
	snap.at, snap.elapsed = time.Now(), time.Since(started)
	return snap, nil
}

// topologyMolecule places a molecule's tasks on its stage graph. A graph
// that does not resolve (a revision this binary cannot run, a content
// mismatch, a failed read) is the molecule's Error, with its tasks listed by
// state alone.
func (d *Daemon) topologyMolecule(ctx context.Context, m controlplane.TopologyMolecule) topologyEntry {
	g, err := d.moleculeGraph(ctx, m.Molecule)
	if err != nil {
		g = moleculeGraph{index: map[string]int{}, err: err.Error()}
	}
	return topologyEntry{molecule: placeTopologyTasks(m.Molecule, g, m.Tasks)}
}

// placeTopologyTasks lists tasks at the stages of a graph, with a stage of
// its own for a state the graph lacks.
func placeTopologyTasks(m controlplane.Molecule, g moleculeGraph, tasks []controlplane.TopologyTask) TopologyMolecule {
	index := make(map[string]int, len(g.index))
	for k, v := range g.index {
		index[k] = v
	}
	stages := make([]TopologyStage, len(g.stages))
	for i, s := range g.stages {
		if s.Workflow != nil {
			cp := *s.Workflow
			s.Workflow = &cp
		}
		s.Tasks = []string{}
		stages[i] = s
	}
	for _, task := range tasks {
		i, ok := index[task.State]
		if !ok {
			i = len(stages)
			index[task.State] = i
			stages = append(stages, TopologyStage{Stage: task.State, Tasks: []string{}})
		}
		stages[i].Tasks = append(stages[i].Tasks, task.ID)
	}
	roles := g.roles
	if roles == nil {
		roles = []string{}
	}
	return TopologyMolecule{Molecule: m, RoleOrder: roles, Stages: stages, Error: g.err}
}

// page is the snapshot's page of running molecules after the cursor,
// optionally of one workflow.
func (s *topologySnapshot) page(workflowID, after string, limit int) ([]TopologyMolecule, string, error) {
	out := []TopologyMolecule{}
	for i := sort.Search(len(s.entries), func(i int) bool { return s.entries[i].molecule.ID > after }); i < len(s.entries); i++ {
		e := s.entries[i]
		if workflowID != "" && e.molecule.WorkflowID != workflowID {
			continue
		}
		if len(out) == limit {
			return out, out[limit-1].ID, nil
		}
		if e.err != nil {
			return nil, "", e.err
		}
		out = append(out, e.molecule)
	}
	return out, "", nil
}

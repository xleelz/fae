package daemon

import (
	"encoding/json"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/kernel"
)

// JobIdentity is the immutable admission response and current read identity.
// Workflow documents, artifact bodies and operator content are never included.
type JobIdentity struct {
	ExecutionKind    string `json:"execution_kind"`
	ID               string `json:"id"`
	Version          int64  `json:"version"`
	WorkflowID       string `json:"workflow_id"`
	WorkflowRevision int64  `json:"workflow_revision"`
	TargetID         string `json:"target_id"`
	TargetRevision   int64  `json:"target_revision"`
	State            string `json:"state"`
}

func identity(j controlplane.JobSnapshot) JobIdentity {
	var marker struct {
		Job struct {
			ExecutionKind string `json:"execution_kind"`
		} `json:"job"`
	}
	_ = json.Unmarshal(j.Payload, &marker)
	return JobIdentity{marker.Job.ExecutionKind, j.ID, j.Version, j.WorkflowID, j.WorkflowRevision, j.TargetID, j.TargetRevision, j.State}
}

type BoardItem struct {
	JobIdentity
	Priority int    `json:"priority"`
	RunID    string `json:"run_id"`
	Mode     string `json:"mode"`
	Reason   string `json:"reason,omitempty"`
	InFlight bool   `json:"in_flight"`
	// EscalationKind is the Appendix B kind of an Escalated task.
	EscalationKind domain.EscalationKind `json:"escalation_kind,omitempty"`
	MoleculeID     string                `json:"molecule_id,omitempty"`
	// PendingDelete is set while a TaskDelete waits to retire the task
	// (daemon/TASKS.md); TaskGet's pending_delete says what it waits for.
	PendingDelete bool              `json:"pending_delete,omitempty"`
	Parent        *engine.ParentRef `json:"parent,omitempty"`
	Child         *engine.ChildRef  `json:"child,omitempty"`
}
type statusSnapshot struct {
	Schedules         map[string]json.RawMessage `json:"-"`
	ScheduleList      json.RawMessage            `json:"-"`
	ControllerUpdated time.Time                  `json:"-"`
	ControllerHealth  json.RawMessage            `json:"-"`
	TicketOutbox      ticketStatus               `json:"ticket_outbox"`
	Maintenance       map[string]any             `json:"maintenance"`
	Drive             map[string]any             `json:"drive"`
	Configuration     map[string]any             `json:"configuration"`
	Scheduler         map[string]any             `json:"scheduler,omitempty"`
	Drained           bool                       `json:"drained"`
	Draining          bool                       `json:"draining"`
	Active            int                        `json:"active"`
	Counts            map[string]int             `json:"counts"`
	Tasks             []BoardItem                `json:"tasks"`
	Quarantined       map[string]string          `json:"quarantined,omitempty"`
	Updated           time.Time                  `json:"updated"`
	// driveJobs is the writer's d.jobs as published, for the reads that
	// must not wait for d.mu (published_tasks.go).
	driveJobs map[string]controlplane.JobSnapshot
}

// publishStatus is called by the serialized writer. Consumers must not mutate
// the published rows or maps, and never wait for SQL or the writer mutex.
func (d *Daemon) publishStatus() {
	d.storeStatus()
	d.refreshStatusExtras()
}

// FAE-ROB-020: cached reads (Board, Schedule, Controller) are refused once the
// snapshot is older than snapshotStaleness. Writer steps republish it, and
// state they do not publish (the ticket outbox's) asks for a re-snapshot when
// it changes. While no writer step runs (an idle or stopped drive) a read that
// finds the snapshot older than statusRefreshAge asks maintainStatusSnapshot
// to re-snapshot the in-memory state off the request path, with no plane read
// and no extras sample; a cached read past the bound waits up to
// statusRefreshWait for it, and status never waits. An idle daemon nobody
// reads does no snapshot work (FAE-ARC-002).
const (
	snapshotStaleness = 10 * time.Second
	statusRefreshAge  = snapshotStaleness / 2
	statusRefreshWait = time.Second
)

// statusRefresher hands requests to maintainStatusSnapshot. done is closed
// once the refresh serving the latest request has finished; dirty forces one.
type statusRefresher struct {
	mu         sync.Mutex
	wake, done chan struct{}
	dirty      bool
}

func (d *Daemon) maintainStatusSnapshot() {
	defer d.workers.Done()
	r := &d.statusRefresh
	r.mu.Lock()
	wake := make(chan struct{}, 1)
	r.wake, r.done = wake, make(chan struct{})
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.wake = nil; close(r.done); r.mu.Unlock() }()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-wake:
		}
		r.mu.Lock()
		done, dirty := r.done, r.dirty
		r.done, r.dirty = make(chan struct{}), false
		r.mu.Unlock()
		if dirty || !d.statusWithin(statusRefreshAge) {
			d.mu.Lock()
			if d.ctx.Err() == nil {
				d.storeStatus()
			}
			d.mu.Unlock()
		}
		close(done)
	}
}

// requestStatus wakes the refresher and returns the channel closed once it has
// served the request (nil when none runs).
func (d *Daemon) requestStatus(changed bool) <-chan struct{} {
	r := &d.statusRefresh
	r.mu.Lock()
	wake, done := r.wake, r.done
	r.dirty = r.dirty || changed
	r.mu.Unlock()
	if wake == nil {
		return nil
	}
	select {
	case wake <- struct{}{}:
	default:
	}
	return done
}

// statusChanged re-snapshots state that changed outside a publishing writer
// step; it never blocks its caller.
func (d *Daemon) statusChanged() { d.requestStatus(true) }

// cachedStatus is the published snapshot, refreshed first as described above
// when no writer step has published it recently. wait is for reads that are
// refused past snapshotStaleness; it must not be set under the writer mutex.
func (d *Daemon) cachedStatus(wait bool) any {
	if !d.statusWithin(statusRefreshAge) {
		if done := d.requestStatus(false); done != nil && wait && !d.statusWithin(snapshotStaleness) {
			timer := time.NewTimer(statusRefreshWait)
			select {
			case <-done:
			case <-timer.C:
			}
			timer.Stop()
		}
	}
	return d.status.Load()
}

func (d *Daemon) statusWithin(age time.Duration) bool {
	s, ok := d.status.Load().(statusSnapshot)
	return ok && time.Since(s.Updated) < age
}

// storeStatus snapshots the writer's in-memory state; it reads no plane row.
func (d *Daemon) storeStatus() {
	s := statusSnapshot{Draining: d.draining, Active: len(d.active) + len(d.loopActive) + len(d.prophetActive), Counts: map[string]int{}, Tasks: []BoardItem{}, Updated: time.Now().UTC()}
	s.driveJobs = maps.Clone(d.jobs)
	if v := d.ticketStatus.Load(); v != nil {
		s.TicketOutbox = v.(ticketStatus)
	}
	if d.ticketActive.Load() {
		s.Active++
	}
	s.Maintenance = d.publicMaintenance()
	s.Drive = map[string]any{"alive": d.driveAlive.Load(), "rounds": d.driveRounds.Load(), "last_transition": time.Unix(0, d.lastTransition.Load()).UTC()}
	s.Drive["generation"] = d.driveGeneration.Load()
	s.Drive["cause"], _ = d.driveCause.Load().(string)
	// The drive publishes after every round; a changed round count dates it.
	s.Drive["last_round"] = nil
	if prev, ok := d.status.Load().(statusSnapshot); ok && prev.Drive != nil {
		s.Drive["last_round"] = prev.Drive["last_round"]
		if prev.Drive["rounds"] != s.Drive["rounds"] {
			s.Drive["last_round"] = s.Updated
		}
	}
	parked := 0
	s.Configuration = map[string]any{"fixed": map[string]any{"instance_root": d.layout.Root, "socket_path": d.layout.Socket}}
	if d.live != nil {
		s.Configuration["dynamic"] = d.dynamicConfiguration()
	}
	s.Scheduler = d.schedulerStatus()
	pending := 0
	if d.backlogUncertain {
		pending++
	}
	for _, s := range d.loops {
		if s.InFlight {
			pending++
		}
	}
	for id, j := range d.jobs {
		row := d.healthRowOf(id, j)
		if !row.decoded {
			continue
		}
		if row.inFlight {
			pending++
		}
		if row.parked {
			parked++
		}
		s.Counts[j.State]++
		item := row.boardItem(j)
		if p := item.Parent; p != nil {
			cp := *p
			item.Parent = &cp
		}
		if c := item.Child; c != nil {
			cp := *c
			if c.Children != nil {
				cp.Children = append([]engine.ChildRef(nil), c.Children...)
			}
			item.Child = &cp
		}
		s.Tasks = append(s.Tasks, item)
	}
	for state, n := range d.storedTerminal {
		if n > 0 {
			s.Counts[state] += n
		}
	}
	if len(d.quarantined) > 0 {
		s.Quarantined = map[string]string{}
		for id, reason := range d.quarantined {
			s.Quarantined[id] = reason
		}
	}
	s.Drive["parked"] = parked
	if len(d.stepFaults) > 0 {
		faults := map[string]string{}
		for step, reason := range d.stepFaults {
			faults[step] = reason
		}
		s.Drive["step_faults"] = faults
	}
	sort.Slice(s.Tasks, func(i, j int) bool { return s.Tasks[i].ID < s.Tasks[j].ID })
	s.Drained = kernel.CanFinishDrain(d.draining, s.Active, pending+s.TicketOutbox.Uncertain)
	s.Schedules = map[string]json.RawMessage{}
	ids := []string{}
	for id, row := range d.loops {
		s.Schedules[id], _ = json.Marshal(row)
		ids = append(ids, id)
	}
	for id, row := range d.crons {
		s.Schedules[id], _ = json.Marshal(row)
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := []json.RawMessage{}
	for _, id := range ids {
		rows = append(rows, s.Schedules[id])
	}
	s.ScheduleList, _ = json.Marshal(rows)
	if d.child != nil {
		s.ControllerHealth, _ = json.Marshal(d.captureControllerHealth(false))
		s.ControllerUpdated = time.Now().UTC()
	}
	d.status.Store(s)
}

// schedulerStatus reports the latest round's withheld tasks by reason
// (FAE-SCH-010) and the workflow-global caps chosen across revisions
// (FAE-SCH-006).
func (d *Daemon) schedulerStatus() map[string]any {
	m := map[string]any{}
	if w := d.withheld.Load(); w != nil {
		m["withheld_round"], m["withheld"], m["withheld_tasks"], m["withheld_totals"] = w.Round, w.Counts, w.Tasks, w.Totals
	}
	if d.live != nil && d.live.guard != nil {
		m["role_caps"] = d.live.guard.schedule().roleCaps()
	}
	if d.idleSessions != nil {
		// Resident transports whose retirement is unproven: each may hold a
		// pane beyond its role's cap until retired (runtime/IDLE_SESSIONS.md).
		m["unretired_transports"] = d.idleSessions.Unretired()
	}
	m["molecules"] = d.moleculeStatus()
	return m
}

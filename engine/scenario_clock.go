package engine

import (
	"fmt"
	"github.com/xleelz/fae/workflow"
	"sort"
	"time"
)

// scenarioClock models ready/running sessions without sleeping or host effects.
// Real host pressure, heavy-op ledgers and target provider pools are exercised
// by the scripted daemon, not inferred from an offline workflow document.
type scenarioClock struct {
	now      time.Duration
	actors   map[string]*scenarioActor
	sequence int
	timeline []workflow.SimulationActivity
	peak     int
}
type scenarioActor struct {
	dispatch Dispatch
	session  int
	order    int
	active   bool
	finish   time.Duration
	activity int
}

func newScenarioClock() *scenarioClock { return &scenarioClock{actors: map[string]*scenarioActor{}} }
func (c *scenarioClock) enqueue(d Dispatch) {
	if _, ok := c.actors[d.ID]; ok {
		return
	}
	c.sequence++
	c.actors[d.ID] = &scenarioActor{dispatch: d, order: c.sequence}
}
func (c *scenarioClock) prune(jobs map[string]Job) {
	for id, a := range c.actors {
		job, ok := jobs[a.dispatch.TaskID]
		found := false
		if ok {
			if job.InFlight != nil && job.InFlight.ID == id {
				found = true
			}
			for _, d := range BranchDispatches(job) {
				if d.ID == id {
					found = true
				}
			}
		}
		if !found {
			if a.active {
				entry := &c.timeline[a.activity]
				entry.FinishMS = float64(c.now) / float64(time.Millisecond)
				entry.Cancelled = true
			}
			delete(c.actors, id)
		}
	}
}
func scenarioSessionConsumes(d Dispatch) bool {
	return d.Config.Primitive == workflow.AgentTurn || d.Config.Primitive == workflow.Review || d.Config.Primitive == workflow.HostCommand
}
func scenarioRolePool(j Job, role string) (string, int) {
	r := j.Document.Roles[role]
	switch r.CapScope {
	case "task":
		return "task/" + j.TaskID + "/" + role, r.Cap
	case "tree":
		return "tree/" + RootTask(j) + "/" + role, r.Cap
	default:
		return "workflow/" + j.Document.ID + "/" + role, r.Cap
	}
}
func (c *scenarioClock) canStart(a *scenarioActor, jobs map[string]Job) bool {
	if !scenarioSessionConsumes(a.dispatch) {
		return true
	}
	job := jobs[a.dispatch.TaskID]
	root := RootTask(job)
	limit := jobs[root].MaxSessions
	role := a.dispatch.Sessions[a.session].Role
	key, cap := scenarioRolePool(job, role)
	if job.Document.Roles[role].CapScope == "tree" {
		for _, member := range jobs {
			if RootTask(member) != root || member.State.Terminal() {
				continue
			}
			r, ok := member.Document.Roles[role]
			if ok && r.CapScope == "tree" && r.Cap > 0 && (cap <= 0 || r.Cap < cap) {
				cap = r.Cap
			}
		}
	}
	usedTree, usedRole := 0, 0
	for _, other := range c.actors {
		if !other.active || !scenarioSessionConsumes(other.dispatch) {
			continue
		}
		member := jobs[other.dispatch.TaskID]
		if RootTask(member) == root {
			usedTree++
		}
		otherKey, _ := scenarioRolePool(member, other.dispatch.Sessions[other.session].Role)
		if otherKey == key {
			usedRole++
		}
	}
	if limit > 0 && usedTree >= limit {
		return false
	}
	if (a.dispatch.Config.Primitive == workflow.AgentTurn || a.dispatch.Config.Primitive == workflow.Review) && cap > 0 && usedRole >= cap {
		return false
	}
	return true
}
func (c *scenarioClock) startReady(jobs map[string]Job, s workflow.Scenario) (bool, error) {
	c.prune(jobs)
	pending := []*scenarioActor{}
	for _, a := range c.actors {
		if !a.active {
			pending = append(pending, a)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].order < pending[j].order })
	started := false
	for _, a := range pending {
		if !c.canStart(a, jobs) {
			continue
		}
		session := a.dispatch.Sessions[a.session]
		rule, _, err := scenarioRule(s, a.dispatch, session.Role)
		if err != nil {
			return false, err
		}
		delay := time.Duration(0)
		if a.session == 0 {
			delay = max(0, rule.Delay)
		}
		if delay > time.Duration(1<<63-1)-c.now {
			return false, fmt.Errorf("scenario virtual duration overflow")
		}
		a.active = true
		a.finish = c.now + delay
		a.activity = len(c.timeline)
		c.timeline = append(c.timeline, workflow.SimulationActivity{Dispatch: a.dispatch.ID, Task: a.dispatch.TaskID, Stage: a.dispatch.Stage, Branch: a.dispatch.BranchID, Role: session.Role, StartMS: float64(c.now) / float64(time.Millisecond), FinishMS: float64(a.finish) / float64(time.Millisecond)})
		active := 0
		for _, other := range c.actors {
			if other.active && scenarioSessionConsumes(other.dispatch) {
				active++
			}
		}
		c.peak = max(c.peak, active)
		started = true
	}
	return started, nil
}

// advance completes the next virtual session. A Review's sessions consume their
// tree and role slots sequentially, matching the scripted adapter's execution.
func (c *scenarioClock) advance() (Dispatch, bool, bool) {
	var next *scenarioActor
	for _, a := range c.actors {
		if a.active && (next == nil || a.finish < next.finish || a.finish == next.finish && a.order < next.order) {
			next = a
		}
	}
	if next == nil {
		return Dispatch{}, false, false
	}
	c.now = next.finish
	next.active = false
	next.session++
	if next.session < len(next.dispatch.Sessions) {
		return Dispatch{}, false, true
	}
	delete(c.actors, next.dispatch.ID)
	return next.dispatch, true, true
}
func (c *scenarioClock) report(result *workflow.Simulation) {
	result.Timeline = append([]workflow.SimulationActivity(nil), c.timeline...)
	result.DurationMS = float64(c.now) / float64(time.Millisecond)
	result.PeakSessions = c.peak
}

package daemon

import (
	"fmt"
	"sync"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// integrationQueue serialises integration onto the target (FAE-VCS-008): one
// task at a time holds the window from its Merge dispatch until it lands or
// leaves for remediation, so Fae's own landings never move the target under a
// task between its post-merge QA and its integration, and a busy mainline does
// not exhaust the bounded retry route. Holding is derived from durable task
// state; only the claim of a Merge dispatch in flight is in memory.
type integrationQueue struct {
	mu      sync.Mutex
	merging map[string]string // task -> claiming Merge dispatch
}

// integrationSegment lists the stages between a Merge and an Integrate that
// hold the window: reachable from a Merge without its re-verify route or a
// change producer, and still leading to an Integrate. Merge stages hold only
// through a dispatch's claim.
func integrationSegment(doc workflow.Document) map[string]bool {
	next := map[string][]string{}
	for _, e := range doc.Edges {
		if s, ok := doc.Stages[e.From]; ok && s.Primitive == workflow.Merge && e.To == s.Reverify {
			continue
		}
		next[e.From] = append(next[e.From], e.To)
	}
	reach := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		s, ok := doc.Stages[id]
		if !ok || reach[id] || s.ChangeProducer || s.Primitive == workflow.Escalate || s.Primitive == workflow.Merge {
			return
		}
		reach[id] = true
		for _, to := range next[id] {
			walk(to)
		}
	}
	for id, s := range doc.Stages {
		if s.Primitive == workflow.Merge {
			for _, to := range next[id] {
				walk(to)
			}
		}
	}
	leads := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for id := range reach {
			if leads[id] {
				continue
			}
			lead := doc.Stages[id].Primitive == workflow.Integrate
			for _, to := range next[id] {
				lead = lead || leads[to]
			}
			if lead {
				leads[id], changed = true, true
			}
		}
	}
	return leads
}

// claim admits a Merge dispatch when no other task holds the window, and
// returns the holder otherwise. An Escalated, terminal or operator-held task
// never holds it; a task deferred only by host admission keeps its place.
func (q *integrationQueue) claim(d *Daemon, task, dispatch string) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	segments := map[string]map[string]bool{}
	for id, snapshot := range d.jobs {
		if id == task || backlogTerminal(snapshot.State) || snapshot.State == string(domain.Escalated) {
			continue
		}
		var e envelope
		if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
			return "", err
		}
		j := e.Job
		if j.ExecutionKind != "live" || j.Parked && j.Reason != "infrastructure-deferred" {
			continue
		}
		segment, ok := segments[j.WorkflowHash]
		if !ok {
			segment = integrationSegment(j.Document)
			segments[j.WorkflowHash] = segment
		}
		if segment[string(j.State)] || q.merging[id] != "" && j.InFlight != nil && j.InFlight.ID == q.merging[id] {
			return id, nil
		}
		delete(q.merging, id)
	}
	if q.merging == nil {
		q.merging = map[string]string{}
	}
	q.merging[task] = dispatch
	return "", nil
}

// integrationTurn defers a Merge dispatch, uncharged, while another task
// holds the integration window.
func (x *liveExecutor) integrationTurn(task, dispatch string) error {
	if x.queue == nil {
		return nil
	}
	holder, err := x.queue.claim(x.daemon, task, dispatch)
	if err != nil || holder == "" {
		return err
	}
	return fmt.Errorf("integration queue: task %s holds %s", holder, x.config.IntegrationRef)
}

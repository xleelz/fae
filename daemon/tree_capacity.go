package daemon

import "github.com/xleelz/fae/workflow"

// A parent awaiting a SubWorkflow performs no agent/command work. Charging its
// orchestration wait against runtime capacity can deadlock its own descendants.
// Caller holds d.mu, as for the existing active map admission guard.
func (d *Daemon) executingTasksInFlight() int {
	n := len(d.loopActive)
	for id := range d.active {
		snapshot, ok := d.jobs[id]
		if ok {
			var e envelope
			if decodeEnvelope(snapshot.Payload, &e) == nil && e.Job.InFlight != nil && e.Job.InFlight.Config.Primitive == workflow.SubWorkflow {
				continue
			}
		}
		n++
	}
	return n
}

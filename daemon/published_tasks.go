package daemon

import (
	"context"
	"errors"

	"github.com/xleelz/fae/controlplane"
)

// Pool reads that need the drive's task checkpoints (Census, ReviewTargetHealth
// and the global Prophet's scope) read them from the status snapshot, never
// from d.jobs under d.mu, so a writer held up in plane, git or ledger I/O
// cannot pin their read-pool slots (FAE-ROB-020).
//
// storeStatus publishes a copy of d.jobs and d.quarantined with each
// snapshot. A change to them is published before its writer releases d.mu
// (every mutating RPC publishes on return, rpc.go), or, for a drive round's
// housekeeping step, at the latest when that round ends. So a read sees every
// change of a returned RPC or a finished round. While a writer holds the
// lock, a read sees the tasks as they were before that writer's changes, as a
// read that took the lock just before it would have. As for status, a read
// that finds the snapshot older than statusRefreshAge asks the keeper to
// re-snapshot, and never waits for it.

// publishedTasks is the published snapshot holding the drive's tasks
// (driveJobs) and quarantine. Callers must not mutate it.
func (d *Daemon) publishedTasks() (statusSnapshot, error) {
	s, ok := d.cachedStatus(false).(statusSnapshot)
	if !ok {
		return statusSnapshot{}, errors.New("drive snapshot unavailable")
	}
	return s, nil
}

// publishedJob is storedJob over a published snapshot, for a caller that
// does not hold d.mu: the drive's own checkpoint, else the plane's row of a
// terminal task startup did not materialise. A quarantined or otherwise
// unloaded non-terminal task stays absent.
func (s statusSnapshot) publishedJob(ctx context.Context, d *Daemon, id string) (controlplane.JobSnapshot, bool, error) {
	if j, ok := s.driveJobs[id]; ok {
		return j, true, nil
	}
	if s.Quarantined[id] != "" {
		return controlplane.JobSnapshot{}, false, nil
	}
	return d.terminalJob(ctx, id)
}

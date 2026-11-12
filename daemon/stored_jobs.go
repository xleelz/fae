package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/xleelz/fae/controlplane"
)

// The drive materialises only the jobs it may still act on: the non-terminal
// ones and terminal ones with an unfinished ticket lifecycle (FAE-CP-009).
// Every other terminal job is read from the plane by key when a verb or read
// names it, and is counted by state in status.

// loadActiveJobs reads the jobs startup materialises and counts the rest.
func (d *Daemon) loadActiveJobs(ctx context.Context) ([]controlplane.JobSnapshot, error) {
	snapshots, err := d.store.ListActiveJobs(ctx, 100000)
	if err != nil {
		return nil, err
	}
	pending, err := d.store.PendingTicketTasks(ctx, 100000)
	if err != nil {
		return nil, err
	}
	ticketed, err := d.store.GetJobs(ctx, pending)
	if err != nil {
		return nil, err
	}
	if d.storedTerminal, err = d.store.TerminalCounts(ctx); err != nil {
		return nil, err
	}
	for _, s := range ticketed {
		d.storedTerminal[s.State]--
	}
	return append(snapshots, ticketed...), nil
}

// storedStates are the terminal states a read admits in which the plane holds
// tasks startup did not materialise. d.storedTerminal is fixed before the
// socket serves, so readers need no lock.
func (d *Daemon) storedStates(admits func(state string) bool) []string {
	states := []string{}
	for _, state := range []string{"Cancelled", "Done", "Retired"} {
		if d.storedTerminal[state] > 0 && admits(state) {
			states = append(states, state)
		}
	}
	return states
}

// storedCount counts the unmaterialised tasks in the given states.
func (d *Daemon) storedCount(states []string) int {
	n := 0
	for _, state := range states {
		n += d.storedTerminal[state]
	}
	return n
}

// storedJob is a task's checkpoint: the drive's own, else the plane's row of
// a terminal task startup did not materialise. A quarantined or otherwise
// unloaded non-terminal task stays absent, as before. The caller holds d.mu;
// a row its RPC read before taking the lock (writerReads) is used in place of
// a plane read.
func (d *Daemon) storedJob(ctx context.Context, id string) (controlplane.JobSnapshot, bool, error) {
	if s, ok := d.jobs[id]; ok {
		return s, true, nil
	}
	if d.quarantined[id] != "" {
		return controlplane.JobSnapshot{}, false, nil
	}
	if row, read := readsBefore(ctx).row(id); read && !d.quarantineHeld(id) {
		return row.snapshot, row.ok, nil
	}
	return d.terminalJob(ctx, id)
}

type writerReadsKey struct{}

// writerReads are the plane reads a mutating RPC makes before it takes the
// writer lock, so that no other writer waits on them (FAE-ROB-020/-021): the
// identities the plane stores, finished tasks' rows and a run's members, each
// read by key, and run-wide resume's active backlog and latest run. They are
// re-checked under the lock, where the drive's own state comes first. Every
// task bound since a read is in d.jobs, d.quarantined or a quarantine
// retirement, or its admission row write returned an error and is read again
// (admissionWriteBook), so a row read here is used only for a task none of
// them holds: one the plane held before the read, such as a finished task
// startup did not materialise, whose row no longer changes. The latest run
// stands only while no admission row was written since it was read, and the
// active backlog serves only for the admission times of tasks whose
// checkpoints predate recording them, which do not change. A failed read is
// not kept; the writer then reads under the lock as before.
type writerReads struct {
	existing map[string]bool
	stored   map[string]storedRow
	members  map[string][]string
	active   *controlplane.ActiveGraph
	latest   *latestRunRead
}

// latestRunRead is the default run read before the writer lock, with the
// count of admission row writes made before it was read.
type latestRunRead struct {
	run    string
	found  bool
	writes uint64
}

type storedRow struct {
	snapshot controlplane.JobSnapshot
	ok       bool
}

func readsBefore(ctx context.Context) *writerReads {
	r, _ := ctx.Value(writerReadsKey{}).(*writerReads)
	return r
}

func (r *writerReads) row(id string) (storedRow, bool) {
	if r == nil {
		return storedRow{}, false
	}
	row, ok := r.stored[id]
	return row, ok
}

func (r *writerReads) existingTask(id string) (stored, read bool) {
	if r == nil {
		return false, false
	}
	stored, read = r.existing[id]
	return stored, read
}

func (r *writerReads) runMembers(run string) ([]string, bool) {
	if r == nil {
		return nil, false
	}
	members, ok := r.members[run]
	return members, ok
}

func (r *writerReads) activeBacklog() (controlplane.ActiveGraph, bool) {
	if r == nil || r.active == nil {
		return controlplane.ActiveGraph{}, false
	}
	return *r.active, true
}

// latestRun is the latest run read before the writer lock, if no admission
// row was written since: writes is the caller's count, read under d.mu.
func (r *writerReads) latestRun(writes uint64) (run string, found, read bool) {
	if r == nil || r.latest == nil || r.latest.writes != writes {
		return "", false, false
	}
	return r.latest.run, r.latest.found, true
}

// readBeforeWriterLock makes the plane reads of Admit, ResumeRun and
// BacklogEdge before the RPC takes d.mu; the RPC finds them in its context.
func (d *Daemon) readBeforeWriterLock(ctx context.Context, method string, raw json.RawMessage) context.Context {
	r := &writerReads{existing: map[string]bool{}, stored: map[string]storedRow{}, members: map[string][]string{}}
	switch method {
	case "Admit":
		d.admitReads(ctx, raw, r)
	case "ResumeRun":
		d.resumeReads(ctx, raw, r)
	case "BacklogEdge":
		d.edgeReads(ctx, raw, r)
	default:
		return ctx
	}
	return context.WithValue(ctx, writerReadsKey{}, r)
}

// uncached returns, under a brief hold of d.mu, the ids the drive neither
// caches nor quarantines, with the test seam for the plane reads that follow
// without the lock.
func (d *Daemon) uncached(ids []string) ([]string, func(name string) error) {
	out, _, seam := d.uncachedSince(ids)
	return out, seam
}

// uncachedSince is uncached that also returns the count of admission row
// writes so far, read in the same hold.
func (d *Daemon) uncachedSince(ids []string) ([]string, uint64, func(name string) error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []string{}
	for _, id := range ids {
		if _, held := d.jobs[id]; !held && !d.quarantineHeld(id) {
			out = append(out, id)
		}
	}
	fault := d.testFault
	return out, d.admissionWrites.writes, func(name string) error {
		if fault == nil {
			return nil
		}
		return fault("plane-read", name)
	}
}

// readExisting reads without d.mu which of ids the plane stores, as
// refuseExistingTask and knownTasks would under it.
func (r *writerReads) readExisting(ctx context.Context, d *Daemon, ids []string) {
	ids, seam := d.uncached(ids)
	if len(ids) == 0 {
		return
	}
	for _, id := range ids {
		if seam("existing/"+id) != nil {
			return
		}
	}
	stored, err := d.store.ExistingJobs(ctx, ids)
	if err != nil {
		return
	}
	for _, id := range ids {
		r.existing[id] = stored[id]
	}
}

// readStored reads without d.mu the rows storedJob would read under it.
func (r *writerReads) readStored(ctx context.Context, d *Daemon, ids []string) {
	ids, seam := d.uncached(ids)
	for _, id := range ids {
		if seam("stored/"+id) != nil {
			return
		}
		s, ok, err := d.terminalJob(ctx, id)
		if err != nil {
			return
		}
		r.stored[id] = storedRow{s, ok}
	}
}

// lockedStoredJob is storedJob for a caller that does not hold d.mu: the
// cache is read under the lock and the plane without it.
func (d *Daemon) lockedStoredJob(ctx context.Context, id string) (controlplane.JobSnapshot, bool, error) {
	d.mu.Lock()
	s, ok := d.jobs[id]
	quarantined := d.quarantined[id] != ""
	d.mu.Unlock()
	if ok || quarantined {
		return s, ok, nil
	}
	return d.terminalJob(ctx, id)
}

func (d *Daemon) terminalJob(ctx context.Context, id string) (controlplane.JobSnapshot, bool, error) {
	s, err := d.store.GetJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !backlogTerminal(s.State) {
		return controlplane.JobSnapshot{}, false, nil
	}
	return s, err == nil, err
}

// knownTasks is every task the drive holds plus the identities, among those
// given, that the plane holds. The caller holds d.mu; an identity its RPC
// read before taking the lock (edgeReads) is not read again unless that read
// no longer stands (existingBeforeLock).
func (d *Daemon) knownTasks(ctx context.Context, edges ...[]controlplane.BacklogEdge) (map[string]bool, error) {
	ids := backlogIDs(d.jobs)
	stored := map[string]bool{}
	unknown := []string{}
	seen := map[string]bool{}
	for _, list := range edges {
		for _, e := range list {
			for _, id := range []string{e.From, e.To} {
				if ids[id] || seen[id] {
					continue
				}
				seen[id] = true
				if found, read := d.existingBeforeLock(ctx, id); read {
					stored[id] = found
				} else {
					unknown = append(unknown, id)
				}
			}
		}
	}
	if len(unknown) > 0 {
		found, err := d.store.ExistingJobs(ctx, unknown)
		if err != nil {
			return nil, err
		}
		for _, id := range unknown {
			stored[id] = found[id]
		}
	}
	for id, found := range stored {
		if found && d.quarantined[id] == "" {
			ids[id] = true
		}
	}
	return ids, nil
}

// blockedDependencies is blockedBacklog over the plane's dependency rows.
func blockedDependencies(edges []controlplane.DependencyEdge) map[string][]string {
	blocked := map[string][]string{}
	for _, e := range edges {
		state := "missing"
		if e.Present {
			state = e.Prerequisite.State
		}
		if !CanReleaseDependent(e.Kind == "blocking", e.Present && headSatisfied(e.Prerequisite)) {
			blocked[e.To] = append(blocked[e.To], e.From+": "+state)
		}
	}
	return blocked
}

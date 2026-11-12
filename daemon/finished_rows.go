package daemon

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// finishedRows caches the Board rows of the tasks that finished before the
// start, which startup leaves on the plane (FAE-CP-009). Those tasks never
// change again, so once built the summary serves Board and status task rows
// from memory, with no plane read and no read-pool slot, however saturated
// the pool is (FAE-ROB-020). It is built lazily, by the first read that needs
// those rows, off the request path; until it is ready such reads page the
// plane in the read pool. It holds one row per task, as the snapshot does for
// the tasks finished since the start.
type finishedRows struct {
	mu       sync.Mutex
	summary  *finishedSummary
	building bool
	failedAt time.Time
	// hold, when set, delays a build until it is closed (a test seam).
	hold chan struct{}
	// read counts the plane rows read for finished tasks, by request paging
	// and by summary builds, and snapshots those of them decoded from their
	// snapshots (FAE-CP-009 instrumentation).
	read, snapshots atomic.Int64
}

type finishedSummary struct {
	rows  []BoardItem // in id order
	sizes []int       // each row's encoded size
}

// finishedRetry spaces summary builds after a failed one; reads page the
// plane meanwhile.
const finishedRetry = 5 * time.Second

// finishedSummary returns the built summary, or nil after starting a build
// when none is ready, running or recently failed.
func (d *Daemon) finishedSummary() *finishedSummary {
	f := &d.finished
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.summary != nil || f.building || time.Since(f.failedAt) < finishedRetry {
		return f.summary
	}
	if d.beginWorker() == nil {
		f.building = true
		go d.buildFinished(f.hold)
	}
	return nil
}

// buildFinished pages every finished task startup left on the plane from the
// job and task columns (TerminalRowsAfter), skipping the tasks the snapshot
// holds, as each read also does when it merges.
func (d *Daemon) buildFinished(hold chan struct{}) {
	defer d.workers.Done()
	sum, err := &finishedSummary{}, error(nil)
	defer func() {
		f := &d.finished
		f.mu.Lock()
		defer f.mu.Unlock()
		f.building = false
		if err != nil {
			f.failedAt = time.Now()
			if d.ctx.Err() == nil {
				d.taskLog("finished-task summary unavailable: %v\n", err)
			}
			return
		}
		f.summary = sum
	}()
	if hold != nil {
		select {
		case <-hold:
		case <-d.ctx.Done():
			err = d.ctx.Err()
			return
		}
	}
	states := d.storedStates(func(string) bool { return true })
	for cursor := ""; len(states) > 0; {
		ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
		page, e := d.store.TerminalRowsAfter(ctx, cursor, 1000, controlplane.TerminalFilter{States: states})
		cancel()
		if err = e; err != nil {
			return
		}
		d.finished.count(page)
		s, _ := d.status.Load().(statusSnapshot)
		for _, r := range page {
			cursor = r.ID
			if snapshotHolds(s, r.ID) {
				continue
			}
			d.hydrateFinishedRow(ctx, &r)
			if row, ok := finishedBoardRow(r); ok {
				raw, _ := json.Marshal(row)
				sum.rows, sum.sizes = append(sum.rows, row), append(sum.sizes, len(raw))
			}
		}
		if len(page) < 1000 {
			return
		}
	}
}

// count records a page of finished rows read from the plane.
func (f *finishedRows) count(page []controlplane.TerminalRow) {
	f.read.Add(int64(len(page)))
	for _, r := range page {
		if !r.Columns {
			f.snapshots.Add(1)
		}
	}
}

// after returns, in id order, the summary's rows after the cursor that keep
// admits and the snapshot lacks, at most want of them (0: all), with their
// total encoded size.
func (sum *finishedSummary) after(s statusSnapshot, keep func(BoardItem) bool, cursor string, want int) ([]BoardItem, int) {
	out, size := []BoardItem{}, 0
	for i := sort.Search(len(sum.rows), func(i int) bool { return sum.rows[i].ID > cursor }); i < len(sum.rows); i++ {
		if row := sum.rows[i]; keep(row) && !snapshotHolds(s, row.ID) {
			out, size = append(out, row), size+sum.sizes[i]
			if want > 0 && len(out) == want {
				break
			}
		}
	}
	return out, size
}

// snapshotHolds reports a task the snapshot holds or has quarantined; its
// plane row is not shown again.
func snapshotHolds(s statusSnapshot, id string) bool {
	i := sort.Search(len(s.Tasks), func(i int) bool { return s.Tasks[i].ID >= id })
	return i < len(s.Tasks) && s.Tasks[i].ID == id || s.Quarantined[id] != ""
}

// hydrateFinishedRow loads the snapshot payload for a finished sub-workflow
// child task when TerminalRowsAfter returned columns without the blob, so
// finishedBoardRow preserves Parent across a daemon restart.
func (d *Daemon) hydrateFinishedRow(ctx context.Context, r *controlplane.TerminalRow) {
	if r == nil || !r.Columns || len(r.Payload) > 0 || d == nil || d.store == nil {
		return
	}
	if !strings.Contains(r.ID, ".") && !strings.HasPrefix(r.ID, "sub-") {
		return
	}
	if job, err := d.store.GetJob(ctx, r.ID); err == nil {
		r.Payload = job.Payload
	}
}

// finishedBoardRow is a finished task's Board row: from the columns when they
// hold it exactly, else decoded from its snapshot. A finished task is never
// in flight: the kernel refuses to cancel or retire a task with a dispatch in
// flight, and a completion clears its dispatch before routing to Done.
func finishedBoardRow(r controlplane.TerminalRow) (BoardItem, bool) {
	if !r.Columns {
		h := decodeHealthRow(r.Payload, false)
		return h.boardItem(r.JobSnapshot), h.decoded
	}
	var parent *engine.ParentRef
	var child *engine.ChildRef
	if len(r.Payload) > 0 {
		h := decodeHealthRow(r.Payload, false)
		parent, child = h.parent, h.child
	}
	return BoardItem{JobIdentity{r.ExecutionKind, r.ID, r.Version, r.WorkflowID, r.WorkflowRevision, r.TargetID, r.TargetRevision, r.State}, r.Priority, r.RunID, r.Mode, r.Reason, false, domain.EscalationKind(r.Escalation), r.MoleculeID, false, parent, child}, true
}

// storedFloor is the least encoded size of the rows of the finished tasks
// startup left on the plane in the given states: each is at least as long as
// its state's empty row. Status task rows past it are refused unread.
func (d *Daemon) storedFloor(states []string) int {
	n := 0
	for _, state := range states {
		raw, _ := json.Marshal(BoardItem{JobIdentity: JobIdentity{State: state}})
		n += d.storedTerminal[state] * len(raw)
	}
	return n
}

// mergeByID merges two id-ordered row lists, at most want rows (0: all).
func mergeByID(visible, stored []BoardItem, want int) []BoardItem {
	merged := make([]BoardItem, 0, len(visible)+len(stored))
	for len(visible) > 0 || len(stored) > 0 {
		if len(stored) == 0 || len(visible) > 0 && visible[0].ID < stored[0].ID {
			merged, visible = append(merged, visible[0]), visible[1:]
		} else {
			merged, stored = append(merged, stored[0]), stored[1:]
		}
		if want > 0 && len(merged) == want {
			break
		}
	}
	return merged
}

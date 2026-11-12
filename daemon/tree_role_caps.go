package daemon

import (
	"context"
	"time"
)

func smallerCap(a, b int) int {
	if b > 0 && (a < 1 || b < a) {
		return b
	}
	return a
}
func (a *liveAdmission) treeRoleCapacity(task, root, name string, own int) (int, error) {
	cap := own
	v := a.schedule()
	if v.treeRoleCap != nil {
		cap = smallerCap(cap, v.treeRoleCap[root+"\x00"+name])
	}
	// Query the indexed tree so live revisions outside this runnable view
	// still contribute. Terminal tasks no longer constrain role capacity.
	if a.daemon != nil && a.daemon.store != nil {
		base := a.daemon.ctx
		if base == nil {
			base = context.Background()
		}
		ctx, cancel := context.WithTimeout(base, 5*time.Second)
		defer cancel()
		rows, err := a.daemon.store.TreeJobs(ctx, task)
		if err != nil {
			return 0, err
		}
		for _, row := range rows {
			var e envelope
			if err := decodeEnvelope(row.Payload, &e); err != nil {
				return 0, err
			}
			if e.Job.State.Terminal() {
				continue
			}
			r, ok := e.Job.Document.Roles[name]
			if ok && r.CapScope == "tree" {
				cap = smallerCap(cap, r.Cap)
			}
		}
	}
	return cap, nil
}

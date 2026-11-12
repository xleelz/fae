package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
)

// leaseWait is one lease wait of a task's dispatch (FAE-OBS-003): it becomes
// a lease-wait event in the task's history and one admission log line.
type leaseWait struct {
	Task, Stage, Dispatch, Lease, Outcome string
	Wait                                  time.Duration
}

func (a *liveAdmission) traceLeaseWait(d engine.Dispatch, lease string, since time.Time, granted bool) {
	w := leaseWait{Task: d.TaskID, Stage: d.Stage, Dispatch: d.ID, Lease: lease, Outcome: "refused", Wait: time.Since(since)}
	if granted {
		w.Outcome = "granted"
	}
	if a.daemon != nil {
		a.daemon.recordLeaseWait(w)
	}
}
func (d *Daemon) recordLeaseWait(w leaseWait) {
	var err error
	if d.store != nil && w.Task != "" {
		payload, _ := json.Marshal(map[string]any{"lease": w.Lease, "stage": w.Stage, "dispatch_id": w.Dispatch, "wait_seconds": w.Wait.Seconds(), "outcome": w.Outcome})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = d.store.RecordTaskEvent(ctx, controlplane.Audit{Actor: "daemon", Action: "lease-wait", Outcome: w.Outcome}, w.Task, controlplane.Event{Kind: leaseWaitEventKind, Payload: payload})
		cancel()
		if err == nil {
			d.notifyEvents()
		}
	}
	if d.obs.leaseWaits != nil {
		d.obs.leaseWaits(w)
	}
	line := fmt.Sprintf("lease wait lease=%s outcome=%s wait=%s %s", w.Lease, w.Outcome, w.Wait.Round(time.Millisecond), logTask(w.Task, w.Stage, w.Dispatch))
	if err != nil {
		line += fmt.Sprintf(" event_error=%q", err.Error())
	}
	d.logLine("admission", "%s\n", line)
}

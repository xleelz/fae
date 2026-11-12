package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/scheduler"
)

// heavyWaitingEventKind is the FAE-SCH-011 clause 4 waiting event.
const heavyWaitingEventKind = "heavy-lease-waiting"

func (a *liveAdmission) queueNow() time.Time {
	if a.queueClock != nil {
		return a.queueClock()
	}
	return time.Now()
}

// waitingHeavy names the task of one of this daemon's heavy waiters, so its
// waiting event joins that task's history.
func (a *liveAdmission) waitingHeavy(id, task string) {
	if _, scheduled := a.schedule().molecule[task]; scheduled {
		a.heavyTasks.Store(id, task)
	}
}

// heavyWaiting reports a heavy-op waiter not admitted within the warning bound
// (FAE-SCH-011 clause 4). The pool marks each waiter once, so its observer
// writes exactly one line naming the wait and the current holders, and the same
// as an event: on the waiting task when it is one of this daemon's, else on the
// instance stream. Events can name any queued request, so the line names only
// the request id.
func (a *liveAdmission) heavyWaiting(event scheduler.Event) {
	holders := event.Holders
	if holders == nil {
		holders = []string{}
	}
	named := strings.Join(holders, ",")
	if named == "" {
		named = "none"
	}
	a.logf("host lease waiting reason=HeavyLease id=%s wait=%s holders=%s\n", event.RequestID, event.Wait, named)
	if a.daemon == nil || a.daemon.store == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{"lease": "heavy-op", "request": event.RequestID, "wait_seconds": event.Wait.Seconds(), "holders": holders})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	audit := controlplane.Audit{Actor: "daemon", Action: heavyWaitingEventKind, Outcome: "waiting"}
	var err error
	if task, ok := a.heavyTasks.Load(event.RequestID); ok {
		err = a.daemon.store.RecordTaskEvent(ctx, audit, task.(string), controlplane.Event{Kind: heavyWaitingEventKind, Payload: payload})
	} else {
		err = a.daemon.store.ControllerAction(ctx, audit, payload, false)
	}
	if err != nil {
		a.logf("host lease waiting event unrecorded id=%s: %v\n", event.RequestID, err)
		return
	}
	a.daemon.notifyEvents()
}

package daemon

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// FAE-OBS-006: every park, every escalation, every binding the bridge
// refused (a route-refused failover) and every dispatch refused before launch
// has exactly one structured supervision line paired with its event row. A
// line is written only after the checkpoint carrying the event is durable,
// so no line exists without its row; every commit path (drive checkpoint,
// its verified lost acknowledgement or later reconciliation, operator
// controls, uncertain-dispatch resolution) calls logParksAndEscalations
// exactly once with the committed events.
const pairedEventComponent = "task"

// refusedBeforeLaunchCausePrefix starts the cause of the failure event a
// dispatch refused before launch records (engine.ClassRefusedBeforeLaunch).
const refusedBeforeLaunchCausePrefix = "RefusedBeforeLaunch: "

// pairedEventLine is the line for a park, escalation, route-refused or
// refused-before-launch event, else false.
func pairedEventLine(e controlplane.Event) (string, bool) {
	var ev engine.Event
	if json.Unmarshal(e.Payload, &ev) != nil {
		return "", false
	}
	switch {
	case ev.Kind == "park":
		return fmt.Sprintf("park reason=%q attempt=%d %s\n", ev.Cause, ev.Attempt, logTask(e.JobID, ev.Stage, ev.DispatchID)), true
	case ev.Kind == "escalation" || ev.Kind == "transition" && ev.To == domain.Escalated:
		return fmt.Sprintf("escalation kind=%s cause=%q attempt=%d %s\n", ev.EscalationKind, ev.Cause, ev.Attempt, logTask(e.JobID, ev.Stage, ev.DispatchID)), true
	case ev.Kind == "route-refused":
		return fmt.Sprintf("route-refused cause=%q attempt=%d %s\n", ev.Cause, ev.Attempt, logTask(e.JobID, ev.Stage, ev.DispatchID)), true
	case ev.Kind == "failure" && strings.HasPrefix(ev.Cause, refusedBeforeLaunchCausePrefix):
		return fmt.Sprintf("refused-before-launch cause=%q attempt=%d %s\n", strings.TrimPrefix(ev.Cause, refusedBeforeLaunchCausePrefix), ev.Attempt, logTask(e.JobID, ev.Stage, ev.DispatchID)), true
	}
	return "", false
}

func (d *Daemon) logParksAndEscalations(events []controlplane.Event) {
	for _, e := range events {
		if line, ok := pairedEventLine(e); ok {
			d.logLine(pairedEventComponent, "%s", line)
		}
	}
}

// logCommittedEvents is persist's deferred hook: lastTransition moves only when
// that checkpoint committed (its save, or a verified lost acknowledgement).
func (d *Daemon) logCommittedEvents(events []controlplane.Event, before int64) {
	if d.lastTransition.Load() != before {
		d.logParksAndEscalations(events)
	}
}

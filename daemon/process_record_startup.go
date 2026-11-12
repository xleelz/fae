package daemon

import (
	"context"
	"encoding/json"

	"github.com/xleelz/fae/controlplane"
)

// emptyProcessRecordRemoved is the audited action, and the event kind, of a
// startup recovery's removal of an empty process record.
const emptyProcessRecordRemoved = "empty-process-record-removed"

// logClearedProcessRecords writes a supervision log line for each empty
// process record a startup recovery removed (processowner.RecoverClearing,
// FAE-ROB-014), so the removal leaves a trace as soon as it happens, before
// the plane is up for the registry of Dolt's own processes.
func (d *Daemon) logClearedProcessRecords(component string, cleared []string) {
	for _, path := range cleared {
		d.logLine(component, "removed empty process record %s: an earlier process left it empty, so it names no process; nothing was signalled or reclaimed\n", path)
	}
}

// auditClearedProcessRecords writes an audit row and an event for each empty
// process record a startup recovery removed (FAE-ROB-014). The plane's own
// registry is recovered before Dolt starts, so its rows are written once the
// plane is up; the session, host-command and UI registries are recovered with
// the plane up.
func (d *Daemon) auditClearedProcessRecords(ctx context.Context, cleared []string) error {
	for _, path := range cleared {
		body, _ := json.Marshal(map[string]string{"path": path, "reason": "an earlier process left this process record empty; it names no process, so nothing was signalled or reclaimed"})
		if err := d.store.ControllerAction(ctx, controlplane.Audit{Actor: "daemon", Action: emptyProcessRecordRemoved, Inputs: body, Outcome: "removed"}, body, false); err != nil {
			return err
		}
	}
	return nil
}

package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
)

// lifecycle records one daemon or Dolt-child lifecycle event (FAE-API-020).
// It is best effort: a plane that cannot take the event is logged, and the
// lifecycle step itself proceeds.
func (d *Daemon) lifecycle(kind string, fields map[string]any) {
	if d.store == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["instance"], fields["pid"] = d.config.Instance, os.Getpid()
	raw, _ := json.Marshal(fields)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if kind == "daemon-starting" {
		fields["started_at"] = d.started
		if _, err := d.store.LastJobEvent(ctx, "", "daemon-ready"); err == nil {
			fields["restart"] = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			d.taskLog("restart timing provenance not read: %v\n", err)
		}
		raw, _ = json.Marshal(fields)
	}
	if err := d.store.RecordLifecycle(ctx, kind, raw); err != nil {
		d.taskLog("lifecycle event %s not recorded: %v\n", kind, err)
		return
	}
	d.notifyEvents()
}

// sessionEvents derives session launched and reaped events from a checkpoint
// that changes the in-flight dispatch (FAE-API-020); they commit with it.
func sessionEvents(before controlplane.JobSnapshot, after engine.Job, instanceRoot string) []controlplane.Event {
	var old envelope
	var prior *engine.Dispatch
	if len(before.Payload) > 0 && decodeEnvelope(before.Payload, &old) == nil {
		prior = old.Job.InFlight
	}
	current := after.InFlight
	events := []controlplane.Event{}
	emit := func(kind string, d *engine.Dispatch) {
		for _, s := range d.Sessions {
			binding := ""
			if s.Route.Provider != "" {
				binding = s.Route.Provider + "/" + s.Route.Model
			}
			raw, _ := json.Marshal(map[string]any{"session": s.ID, "role": s.Role, "binding": binding, "dispatch_id": d.ID, "stage": d.Stage, "attempt": d.Attempt, "visit": d.Visit, "lease_id": fmt.Sprintf("%x", sha256.Sum256([]byte(instanceRoot+"\x00"+s.ID)))})
			events = append(events, controlplane.Event{Kind: kind, JobID: after.TaskID, Payload: raw})
		}
	}
	if prior != nil && (current == nil || current.ID != prior.ID) {
		emit("session-reaped", prior)
	}
	if current != nil && (prior == nil || prior.ID != current.ID) {
		emit("session-launched", current)
	}
	return events
}

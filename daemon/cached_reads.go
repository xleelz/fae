package daemon

import (
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"time"
)

// Cached copies are built by the writer; RPC handlers never acquire its mutex.
func (d *Daemon) cachedRead(r api.Request) (any, bool, error) {
	if r.Method != "Schedule" && r.Method != "Controller" {
		return nil, false, nil
	}
	permission, _, _ := permissionFor(r)
	if !permission.Read {
		return nil, false, nil
	}
	value := d.status.Load()
	if value == nil {
		return nil, true, &api.RPCError{Code: "snapshot-stale", Message: "snapshot unavailable"}
	}
	snapshot := value.(statusSnapshot)
	if time.Since(snapshot.Updated) > 10*time.Second {
		return nil, true, &api.RPCError{Code: "snapshot-stale", Message: "snapshot exceeds 10 seconds"}
	}
	if r.Method == "Schedule" {
		var p ScheduleParams
		if err := decode(r.Params, &p); err != nil {
			return nil, true, err
		}
		if p.Spec != nil || p.Cron != nil {
			return nil, true, errors.New("specifications only accepted for create")
		}
		if p.Action == "list" {
			return snapshot.ScheduleList, true, nil
		}
		row, ok := snapshot.Schedules[p.ID]
		if !ok {
			return nil, true, errors.New("schedule not found")
		}
		return row, true, nil
	}
	var p controller.Params
	if err := decode(r.Params, &p); err != nil {
		return nil, true, err
	}
	if len(snapshot.ControllerHealth) == 0 {
		return nil, true, errors.New("controller snapshot unavailable")
	}
	var health map[string]any
	if err := json.Unmarshal(snapshot.ControllerHealth, &health); err != nil {
		return nil, true, err
	}
	health["snapshot_age_ms"] = max(0, time.Since(snapshot.ControllerUpdated).Milliseconds())
	return health, true, nil
}

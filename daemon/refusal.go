package daemon

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
)

// FAE-API-011: mutating RPCs are refused with typed errors once shutdown
// begins and while a maintenance marker holds the instance.
func shuttingDownError() error {
	return &api.RPCError{Code: "shutting-down", Message: "daemon is shutting down"}
}
func maintenanceError(cause error) error {
	if cause != nil {
		return &api.RPCError{Code: "maintenance", Message: "maintenance marker unreadable: " + cause.Error()}
	}
	return &api.RPCError{Code: "maintenance", Message: "instance is under maintenance"}
}

// maintenanceState is the daemon's copy of the plane's controller-maintenance
// marker, the single maintenance authority (FAE-GEN-011). It is loaded at
// startup and replaced under d.mu right after each committed change, before
// the file mirror is written; the mirror serves only offline readers (the
// watchdog and the launcher's start), never the gate.
type maintenanceState struct {
	raw  json.RawMessage // the last committed marker; empty or null for none
	held bool
	err  error // why held without a committed holding marker
}

// maintenanceHolds is the marker predicate: prepare until acceptance holds the
// instance; a failed preflight leaves the daemon running and does not.
func maintenanceHolds(m *controller.Marker) bool {
	return m != nil && m.State != "preflight-failed" && m.State != "accepted"
}

func decodeMaintenance(raw json.RawMessage) (*controller.Marker, error) {
	var m *controller.Marker
	if len(raw) == 0 {
		return nil, nil
	}
	if e := json.Unmarshal(raw, &m); e != nil {
		return nil, e
	}
	if m != nil && (m.ID == "" || m.Message == "") {
		return nil, errors.New("invalid durable maintenance marker")
	}
	return m, nil
}

// adoptMaintenance records a committed plane marker; callers hold d.mu.
func (d *Daemon) adoptMaintenance(raw json.RawMessage) error {
	m, e := decodeMaintenance(raw)
	d.maintenance.Store(&maintenanceState{raw: raw, held: e != nil || maintenanceHolds(m), err: e})
	return e
}

// maintenanceUncertain records a marker commit whose outcome is unknown: it
// holds the instance if either the last committed marker or the attempted one
// does, until a later commit (or a restart reading the plane) settles it.
func (d *Daemon) maintenanceUncertain(attempted *controller.Marker, cause error) {
	next := maintenanceState{}
	if prior := d.maintenance.Load(); prior != nil {
		next = *prior
	}
	if !next.held && maintenanceHolds(attempted) {
		next.held, next.err = true, fmt.Errorf("maintenance marker commit outcome unknown: %w", cause)
	}
	d.maintenance.Store(&next)
}

// maintenanceMarker returns a private copy of the current maintenance: the
// last committed marker, or none once it was accepted (the relaunch is done
// and its mirror removed).
func (d *Daemon) maintenanceMarker() (*controller.Marker, error) {
	s := d.maintenance.Load()
	if s == nil {
		return nil, nil
	}
	m, e := decodeMaintenance(s.raw)
	if m != nil && m.State == "accepted" {
		m = nil
	}
	return m, e
}

// maintenanceHeld is the single maintenance predicate (see maintenanceState).
// A daemon that has not restored the plane marker holds nothing. Callers
// racing marker commits hold d.mu.
func (d *Daemon) maintenanceHeld() (bool, error) {
	s := d.maintenance.Load()
	if s == nil {
		return false, nil
	}
	return s.held, s.err
}

// The maintenance stop/start lifecycle and the watchdog's supervision run
// under their own marker; each action checks the marker state itself.
var maintenanceVerbs = map[string]bool{"Controller.prepare": true, "Controller.stop": true, "Controller.accept": true, "Controller.abort": true, "Controller.preflight-failed": true, "Controller.alert": true, "Controller.watchdog-register": true}

// mutationRefusal gates a request before authorization or any write. Reads
// answer under maintenance. It reads only the context and the in-memory plane
// marker, so an early check may run without d.mu; the definitive check holds it.
func (d *Daemon) mutationRefusal(verb string, read bool) error {
	if d.ctx.Err() != nil {
		return shuttingDownError()
	}
	if read || maintenanceVerbs[verb] {
		return nil
	}
	if held, e := d.maintenanceHeld(); held {
		return maintenanceError(e)
	}
	return nil
}

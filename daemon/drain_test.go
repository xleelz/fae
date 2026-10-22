package daemon

import (
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"testing"
)

func TestDrainDoesNotHideUncertainPendingDispatch(t *testing.T) {
	e := envelope{Job: engine.Job{TaskID: "uncertain", Parked: true, InFlight: &engine.Dispatch{ID: "uncollected"}}}
	raw, _ := json.Marshal(e)
	d := &Daemon{draining: true, active: map[string]bool{}, jobs: map[string]controlplane.JobSnapshot{"uncertain": {ID: "uncertain", Payload: raw}}}
	d.publishStatus()
	if d.status.Load().(statusSnapshot).Drained {
		t.Fatal("uncertain pending turn reported drained")
	}
	e.Job.InFlight = nil
	raw, _ = json.Marshal(e)
	d.jobs["uncertain"] = controlplane.JobSnapshot{ID: "uncertain", Payload: raw}
	d.publishStatus()
	if !d.status.Load().(statusSnapshot).Drained {
		t.Fatal("collected turn not reported drained")
	}
}

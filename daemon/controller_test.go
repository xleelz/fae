package daemon

import (
	"encoding/json"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func controllerCall(t *testing.T, d *Daemon, p controller.Params) (controller.Health, error) {
	t.Helper()
	response, e := call(t, d, "Controller", p, "")
	if e != nil {
		return controller.Health{}, e
	}
	raw, _ := json.Marshal(response.Result)
	var h controller.Health
	e = json.Unmarshal(raw, &h)
	return h, e
}
func TestControllerMaintenanceAuditedDrainRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	gated := &drainExecutor{started: make(chan string, 2), release: make(chan struct{}), delegate: engine.NewScripted(defaults.HappyScript())}
	cfg.Executor = gated
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { stop(t, d) })
	if _, e = controllerCall(t, d, controller.Params{Action: "prepare", AuthorizingMessage: "missing", Note: "planned", Marker: &controller.Marker{Launcher: "/private/tmp/launcher.sock"}}); e == nil {
		t.Fatal("unauthorizedmaintenance")
	}
	authorize(t, d)
	if _, e = call(t, d, "Admit", AdmitParams{ID: "work", Tree: "tree", AuthorizingMessage: "message"}, "work"); e != nil {
		t.Fatal(e)
	}
	select {
	case <-gated.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	h, e := controllerCall(t, d, controller.Params{Action: "prepare", AuthorizingMessage: "message", Note: "planned update", Marker: &controller.Marker{Launcher: "/private/tmp/launcher.sock"}})
	if e != nil || h.Maintenance == nil || h.InFlight != 1 {
		t.Fatalf("%+v %v", h, e)
	}
	marker := h.Maintenance
	published := d.status.Load().(statusSnapshot)
	safe, _ := json.Marshal(published.Maintenance)
	if published.Maintenance == nil || strings.Contains(string(safe), "Environment") || strings.Contains(string(safe), "arguments") {
		t.Fatal("unsafe or missing public maintenance summary")
	}
	if _, e = os.Stat(d.controllerMarkerPath()); e != nil {
		t.Fatal(e)
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "stop", AuthorizingMessage: "message", Marker: marker}); e == nil {
		t.Fatal("stoppedactivework")
	}
	if _, e = call(t, d, "Admit", AdmitParams{ID: "late", Tree: "tree", AuthorizingMessage: "message"}, "late"); e == nil {
		t.Fatal("newworkduringmaintenance")
	}
	close(gated.release)
	deadline := time.Now().Add(5 * time.Second)
	for d.status.Load().(statusSnapshot).Active != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "stop", AuthorizingMessage: "message", Marker: marker}); e != nil {
		t.Fatal(e)
	}
	stop(t, d)
	if e = os.Remove(d.controllerMarkerPath()); e != nil {
		t.Fatal(e)
	} // committed SQL, missing filesystem mirror
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if !d.status.Load().(statusSnapshot).Draining {
		t.Fatal("restartignoredmaintenance")
	}
	marker, e = controller.ReadMarker(d.controllerMarkerPath())
	if e != nil || marker.State != "stopped" {
		t.Fatal(marker, e)
	}
	h, e = controllerCall(t, d, controller.Params{Action: "health", AuthorizingMessage: "message"})
	if e != nil || !h.Consistent || h.EnvironmentHash != controller.EnvironmentHash(os.Environ()) {
		t.Fatal(h, e)
	}
	wrong := *marker
	wrong.ID = "forged"
	if _, e = controllerCall(t, d, controller.Params{Action: "accept", AuthorizingMessage: "message", Marker: &wrong}); e == nil {
		t.Fatal("acceptedwrongmaintenanceidentity")
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "accept", AuthorizingMessage: "message", Marker: marker}); e == nil {
		t.Fatal("acceptedoldprocessidentity")
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "abort", AuthorizingMessage: "message", Note: "operator: inspect and retry"}); e != nil {
		t.Fatal(e)
	}
	events, e := d.store.Events(ctx, 0, 500)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, event := range events {
		if event.Kind == "controller-maintenance-failed" {
			found = true
		}
	}
	if !found {
		t.Fatal("aborteventmissing")
	}
}
func TestControllerRejectsUngatedBinaryEvenAcknowledged(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	binary := filepath.Join(cfg.Workspace, "other-binary")
	if e = os.WriteFile(binary, []byte("not the running source"), 0700); e != nil {
		t.Fatal(e)
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "prepare", AuthorizingMessage: "message", Note: "roll", Binary: binary, AckOverlap: true, Marker: &controller.Marker{Launcher: "/private/tmp/launcher.sock"}}); e == nil {
		t.Fatal("ackoverlapbypassedgatedsource")
	}
	if _, e = os.Stat(d.controllerMarkerPath()); !os.IsNotExist(e) {
		t.Fatal("failedgatedproofcreatedmarker")
	}
}

func TestControllerQueuedEffectStageGuard(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, stage := range []string{"commit", "integrate"} {
		job, e := engine.New("queued", 1, defaults.Workflow(), "tree")
		if e != nil {
			t.Fatal(e)
		}
		job.State = domain.State(stage)
		raw, _ := json.Marshal(envelope{Job: job})
		d.jobs["queued"] = controlplane.JobSnapshot{ID: "queued", Payload: raw}
		health := d.controllerHealth()
		if len(health.Unsafe) != 1 || health.Unsafe[0] != "queued" {
			t.Fatalf("queued %s did not hold maintenance: %+v", stage, health)
		}
	}
	delete(d.jobs, "queued")
	for _, primitive := range []workflow.Primitive{workflow.VcsCommit, workflow.Integrate, workflow.ExternalEffect} {
		if !maintenanceEffect(primitive) {
			t.Fatal(primitive)
		}
	}
	if maintenanceEffect(workflow.AgentTurn) || maintenanceEffect(workflow.Review) {
		t.Fatal("ordinary stages block maintenance")
	}
}

func TestControllerFinishesDrainAtQueuedEffectCheckpoint(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	health, e := controllerCall(t, d, controller.Params{Action: "prepare", AuthorizingMessage: "message", Note: "planned", Marker: &controller.Marker{Launcher: "/private/tmp/launcher.sock"}})
	if e != nil || health.Maintenance == nil {
		t.Fatal(e)
	}
	// An already running preceding turn may finish into commit while drain is
	// collecting results. The stage is durable; no effect dispatch has started.
	job, e := engine.New("queued", 1, defaults.Workflow(), "tree")
	if e != nil {
		t.Fatal(e)
	}
	job.State = domain.State("commit")
	raw, _ := json.Marshal(envelope{Job: job})
	d.mu.Lock()
	d.jobs["queued"] = controlplane.JobSnapshot{ID: "queued", Payload: raw}
	d.publishStatus()
	d.mu.Unlock()
	health, e = controllerCall(t, d, controller.Params{Action: "health"})
	if e != nil || health.InFlight != 0 || len(health.Unsafe) != 1 {
		t.Fatalf("%+v %v", health, e)
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "stop", AuthorizingMessage: "message", Marker: health.Maintenance}); e != nil {
		t.Fatalf("authorized drain deadlocked on queued checkpoint: %v", e)
	}
	d.mu.Lock()
	delete(d.jobs, "queued")
	d.mu.Unlock()
}

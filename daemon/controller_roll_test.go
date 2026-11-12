package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

// stageGate blocks one named stage of the scripted pipeline until released.
type stageGate struct {
	stage    string
	reached  chan string
	release  chan struct{}
	delegate *engine.Scripted
}

func (x *stageGate) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	if d.Stage == x.stage {
		x.reached <- d.TaskID
		select {
		case <-ctx.Done():
			return engine.Result{}, ctx.Err()
		case <-x.release:
		}
	}
	return x.delegate.Execute(ctx, d)
}

func jobEnvelope(t *testing.T, d *Daemon, ctx context.Context, id string) envelope {
	t.Helper()
	snapshot, e := d.store.GetJob(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	var env envelope
	if e = decodeEnvelope(snapshot.Payload, &env); e != nil {
		t.Fatal(e)
	}
	return env
}

func TestRollWhenClearWaitsForTaskAtQA(t *testing.T) {
	cfg, ctx := fixture(t)
	gate := &stageGate{stage: "qa", reached: make(chan string, 1), release: make(chan struct{}), delegate: engine.NewScripted(defaults.HappyScript())}
	cfg.Executor = gate
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, e = call(t, d, "Admit", AdmitParams{ID: "at-qa", Tree: "tree", AuthorizingMessage: "message"}, "at-qa"); e != nil {
		t.Fatal(e)
	}
	select {
	case <-gate.reached:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	client := controller.Client{Poll: 5 * time.Millisecond, MarkerPath: d.controllerMarkerPath(), RPC: func(ctx context.Context, p controller.Params) (controller.Health, error) {
		return controllerCall(t, d, p)
	}}
	_, e = client.Stop(ctx, controller.Options{Message: "message", Note: "roll when clear", Launcher: "/private/tmp/launcher.sock", WhenClear: true, MaxWait: 150 * time.Millisecond})
	if e == nil || !strings.Contains(e.Error(), "commit/QA window wait expired") {
		t.Fatal("roll --when-clear did not wait for the task at QA", e)
	}
	if marker, _ := controller.ReadMarker(d.controllerMarkerPath()); marker == nil || marker.State != "preflight-failed" || d.status.Load().(statusSnapshot).Draining {
		t.Fatal("expired QA wait must leave the daemon running with an explicit failure record", marker)
	}
	close(gate.release)
	awaitState(t, d, "at-qa", "Done")
}

func TestPlannedRollChargesNoTaskBudget(t *testing.T) {
	cfg, ctx := fixture(t)
	gated := &drainExecutor{started: make(chan string, 4), release: make(chan struct{}), delegate: engine.NewScripted(defaults.HappyScript())}
	cfg.Executor = gated
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	authorize(t, d)
	if _, e = call(t, d, "Admit", AdmitParams{ID: "rolled", Tree: "tree", AuthorizingMessage: "message"}, "rolled"); e != nil {
		t.Fatal(e)
	}
	select {
	case <-gated.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	h, e := controllerCall(t, d, controller.Params{Action: "prepare", AuthorizingMessage: "message", Note: "planned roll", Marker: &controller.Marker{Launcher: "/private/tmp/launcher.sock"}})
	if e != nil || h.Maintenance == nil || h.InFlight != 1 {
		t.Fatal(h, e)
	}
	close(gated.release)
	for deadline := time.Now().Add(10 * time.Second); d.status.Load().(statusSnapshot).Active != 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	before := jobEnvelope(t, d, ctx, "rolled")
	if _, e = controllerCall(t, d, controller.Params{Action: "stop", AuthorizingMessage: "message", Marker: h.Maintenance}); e != nil {
		t.Fatal(e)
	}
	stop(t, d)
	cfg.Executor = engine.NewScripted(defaults.HappyScript())
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	after := jobEnvelope(t, d, ctx, "rolled")
	if after.Job.Attempts != before.Job.Attempts || !maps.Equal(after.Job.Failures, before.Job.Failures) || !maps.Equal(after.Job.Counts, before.Job.Counts) || after.Job.InFlight != nil || len(after.Job.History) != len(before.Job.History) {
		t.Fatalf("planned restart charged task budget: before %+v after %+v", before.Job, after.Job)
	}
	// Model the replacement process: the restarted daemon is a new identity.
	marker, e := controller.ReadMarker(d.controllerMarkerPath())
	if e != nil || marker == nil || marker.State != "stopped" {
		t.Fatal(marker, e)
	}
	marker.OldPID, marker.OldBirth = 1, "replaced-process"
	raw, _ := json.Marshal(marker)
	if e = d.store.PutMarker(ctx, controlplane.Audit{Actor: "test", Action: "model replacement identity", Outcome: "ok"}, "controller-maintenance", raw); e != nil {
		t.Fatal(e)
	}
	if e = writePrivate(d.controllerMarkerPath(), raw); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	e = d.adoptMaintenance(raw)
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	awaitDrive(t, d, true, 1)
	accepted, e := controllerCall(t, d, controller.Params{Action: "accept", AuthorizingMessage: "message", Marker: marker})
	if e != nil || accepted.Maintenance != nil {
		t.Fatal("post-start acceptance refused", accepted, e)
	}
	if _, e = os.Stat(d.controllerMarkerPath()); !os.IsNotExist(e) {
		t.Fatal("accepted roll left its marker")
	}
	if _, e = call(t, d, "Admit", AdmitParams{ID: "unrolled", Tree: "tree", AuthorizingMessage: "message"}, "unrolled"); e != nil {
		t.Fatal(e)
	}
	awaitState(t, d, "rolled", "Done")
	awaitState(t, d, "unrolled", "Done")
	rolled, baseline := jobEnvelope(t, d, ctx, "rolled"), jobEnvelope(t, d, ctx, "unrolled")
	if rolled.Job.Attempts != baseline.Job.Attempts || !maps.Equal(rolled.Job.Failures, baseline.Job.Failures) || !maps.Equal(rolled.Job.Counts, baseline.Job.Counts) {
		t.Fatalf("rolled task spent more budget than an unrolled one: rolled %+v baseline %+v", rolled.Job, baseline.Job)
	}
	for _, event := range rolled.Job.History {
		if strings.Contains(event.Kind, "recovery") {
			t.Fatal("planned roll recorded a recovery", event)
		}
	}
}

package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeploymentDurableOverlapHolds(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	d.mu.Lock()
	defer d.mu.Unlock()
	job, e := engine.New("held", 1, defaults.Workflow(), "tree")
	if e != nil {
		t.Fatal(e)
	}
	job.State = domain.State(job.Document.Start)
	env := envelope{Job: job, Admission: AdmitParams{ID: "held", Priority: 20}}
	payload, _ := json.Marshal(env)
	d.jobs["held"] = controlplane.JobSnapshot{ID: "held", Payload: payload}
	binary := filepath.Join(t.TempDir(), "binary")
	os.WriteFile(binary, []byte("fixture"), 0600)
	p := controller.Params{Binary: binary, GatedRef: "refs/fae/gated/test", OverlapWait: time.Nanosecond}
	audit := controlplane.Audit{Actor: "test", Action: "hold", Outcome: "ok"}
	h := controller.Health{Overlap: []string{"held"}}
	if e = d.deploymentHolds(ctx, p, &h, audit); e != nil {
		t.Fatal(e)
	}
	if len(h.Overlap) != 0 || len(h.Holds) != 1 {
		t.Fatal(h)
	}
	since := h.Holds[0].Since
	// Repeated polling preserves the original durable timestamp.
	h = controller.Health{Overlap: []string{"held"}}
	if e = d.deploymentHolds(ctx, p, &h, audit); e != nil || !h.Holds[0].Since.Equal(since) {
		t.Fatal(h, e)
	}
	env.Admission.Critical = true
	payload, _ = json.Marshal(env)
	d.jobs["held"] = controlplane.JobSnapshot{ID: "held", Payload: payload}
	h = controller.Health{Overlap: []string{"held"}}
	if e = d.deploymentHolds(ctx, p, &h, audit); e != nil || len(h.Overlap) != 1 {
		t.Fatal(h, e)
	}
	env.Admission.Critical = false
	env.Job.Attempts = env.Job.Document.Limits.Attempts - 1
	payload, _ = json.Marshal(env)
	d.jobs["held"] = controlplane.JobSnapshot{ID: "held", Payload: payload}
	h = controller.Health{Overlap: []string{"held"}}
	if e = d.deploymentHolds(ctx, p, &h, audit); e != nil || !h.Holds[0].TopNearLimit || len(h.Overlap) != 1 {
		t.Fatal(h, e)
	}
	env.Job.TaskAttemptLimit = env.Job.Document.Limits.Attempts + 10
	payload, _ = json.Marshal(env)
	d.jobs["held"] = controlplane.JobSnapshot{ID: "held", Payload: payload}
	h = controller.Health{Overlap: []string{"held"}}
	if e = d.deploymentHolds(ctx, p, &h, audit); e != nil || h.Holds[0].TopNearLimit || len(h.Overlap) != 0 {
		t.Fatal("raised task limit did not update protected overlap", h, e)
	}
	env.Job.TaskAttemptLimit = 0
	payload, _ = json.Marshal(env)
	d.jobs["held"] = controlplane.JobSnapshot{ID: "held", Payload: payload}
	p.AckOverlap = true
	h = controller.Health{Overlap: []string{"held"}}
	if e = d.deploymentHolds(ctx, p, &h, audit); e != nil || len(h.Overlap) != 0 {
		t.Fatal(h, e)
	}
	p.AckOverlap = false
	env.Job.Attempts = 0
	payload, _ = json.Marshal(env)
	d.jobs["held"] = controlplane.JobSnapshot{ID: "held", Payload: payload}
	h = controller.Health{Overlap: []string{"held"}, Holds: []controller.OverlapHold{{Task: "held", QueryFailed: true}}}
	if e = d.deploymentHolds(ctx, p, &h, audit); e != nil || len(h.Overlap) != 1 {
		t.Fatal(h, e)
	}
	delete(d.jobs, "held")
}

func TestDeploymentPreflightFailureIsAuditedWithoutStopping(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	client := controller.Client{Poll: time.Millisecond, RPC: func(ctx context.Context, p controller.Params) (controller.Health, error) {
		return controllerCall(t, d, p)
	}}
	_, e = client.Stop(ctx, controller.Options{Message: "message", Note: "newbuild", Launcher: "/private/tmp/launcher.sock", Binary: "/missing/fae", MaxWait: time.Second})
	if e == nil {
		t.Fatal("missing binary accepted")
	}
	marker, e := controller.ReadMarker(d.controllerMarkerPath())
	if e != nil || marker == nil || marker.State != "preflight-failed" || marker.Launch.SHA256 == "" {
		t.Fatal(marker, e)
	}
	if d.status.Load().(statusSnapshot).Draining {
		t.Fatal("preflight failure drained healthy daemon")
	}
	var alerts int
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "check deployment alert", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM alerts WHERE source='controller'").Scan(&alerts)
	}); e != nil || alerts != 1 {
		t.Fatal(alerts, e)
	}
	// A failed preflight is historical evidence, not active maintenance. The
	// existing daemon remains independently recoverable by its watchdog.
	healthy := awaitDrive(t, d, true, 1)
	if _, e = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); e != nil {
		t.Fatal(e)
	}
	awaitDrive(t, d, false, healthy.DriveGeneration)
	watchdog := controller.Watchdog{RPC: client.RPC, MarkerPath: d.controllerMarkerPath(), StatePath: filepath.Join(d.layout.Run, "preflight-watchdog.json"), Message: "message"}
	if e = watchdog.Tick(ctx, time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	recovered := awaitDrive(t, d, true, healthy.DriveGeneration+1)
	if recovered.PID != healthy.PID || recovered.DoltPID != healthy.DoltPID {
		t.Fatal("drive recovery replaced healthy processes")
	}
	marker, e = controller.ReadMarker(d.controllerMarkerPath())
	if e != nil || marker == nil || marker.State != "preflight-failed" {
		t.Fatal("lost preflight failure evidence", marker, e)
	}
	// A new authorized attempt replaces the failed marker through normal guards.
	h, e := controllerCall(t, d, controller.Params{Action: "prepare", AuthorizingMessage: "message", Note: "retry", Marker: &controller.Marker{Launcher: "/private/tmp/launcher.sock"}})
	if e != nil || h.Maintenance == nil || h.Maintenance.State != "draining" {
		t.Fatal(h, e)
	}
	for _, action := range []string{"stop-drive", "rearm"} {
		if _, e = controllerCall(t, d, controller.Params{Action: action, AuthorizingMessage: "message"}); e == nil {
			t.Fatal("active maintenance allowed drive mutation", action)
		}
	}
}

func TestDeploymentRejectsReplacedStartupExecutable(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	// Model atomic replacement of the executable pathname without modifying the
	// test runner itself. The daemon retains the hash captured before replacement.
	path := filepath.Join(t.TempDir(), "fae")
	if e = os.WriteFile(path, []byte("original executable"), 0700); e != nil {
		t.Fatal(e)
	}
	original, _ := controller.HashFile(path)
	d.mu.Lock()
	oldPath, oldHash := d.binaryPath, d.binaryHash
	d.binaryPath, d.binaryHash = path, original
	d.publishStatus() // Publish the fixture startup identity before replacing its bytes.
	d.mu.Unlock()
	defer func() { d.mu.Lock(); d.binaryPath, d.binaryHash = oldPath, oldHash; d.mu.Unlock() }()
	if e = os.WriteFile(path, []byte("replacement executable"), 0700); e != nil {
		t.Fatal(e)
	}
	replaced, _ := controller.HashFile(path)
	// Read-only health returns the captured startup identity and cached logical
	// consistency. Executable byte validation belongs to the mutating preflight.
	h, e := controllerCall(t, d, controller.Params{Action: "health", AuthorizingMessage: "message"})
	if e != nil || h.BinaryHash != original || h.BinaryHash == replaced || !h.Consistent {
		t.Fatal(h, e)
	}
	client := controller.Client{RPC: func(ctx context.Context, p controller.Params) (controller.Health, error) {
		return controllerCall(t, d, p)
	}}
	_, e = client.Stop(ctx, controller.Options{Message: "message", Note: "same path roll", Binary: path, Launcher: "/private/tmp/launcher.sock", MaxWait: time.Second})
	if e == nil || !strings.Contains(e.Error(), "changed since startup") {
		t.Fatal(e)
	}
	marker, e := controller.ReadMarker(d.controllerMarkerPath())
	if e != nil || marker == nil || marker.State != "preflight-failed" || marker.Launch.SHA256 != original || marker.Replacement != nil {
		t.Fatal(marker, e)
	}
	if d.status.Load().(statusSnapshot).Draining {
		t.Fatal("replacement was allowed to stop daemon")
	}
	// Restoring exactly the startup bytes allows the normal safe retry.
	if e = os.WriteFile(path, []byte("original executable"), 0700); e != nil {
		t.Fatal(e)
	}
	h, e = controllerCall(t, d, controller.Params{Action: "prepare", AuthorizingMessage: "message", Note: "restored", Binary: path, Marker: &controller.Marker{Launcher: "/private/tmp/launcher.sock"}})
	if e != nil || h.Maintenance == nil || h.Maintenance.State != "draining" || h.Maintenance.Launch.SHA256 != original {
		t.Fatal(h, e)
	}
}

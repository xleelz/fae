package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

// FAE-CTL-020: a planted retired row is never counted as in flight (the
// watchdog's allowlist is the molecule's stages), so it raises no stall,
// while a task in a stage is counted; and the durable re-arm budget is
// configurable.
func TestWatchdogIgnoresPlantedRetiredRowAndConfiguredRearmBudget(t *testing.T) {
	cfg, ctx := fixture(t)
	gate := &stageGate{stage: "qa", reached: make(chan string, 1), release: make(chan struct{}), delegate: engine.NewScripted(defaults.HappyScript())}
	cfg.Executor, cfg.RearmBudget, cfg.RearmWindow = gate, 1, time.Hour
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	defer close(gate.release)
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	if _, err = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	awaitDrive(t, d, false, 1)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "planted", Tree: "tree", AuthorizingMessage: "message"}, "planted"); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"Escalate", "Retire"} {
		if _, err = call(t, d, method, TaskControlParams{ID: "planted", Reason: "obsolete ruling-7", AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(method, err)
		}
	}
	if s, _ := d.store.GetJob(ctx, "planted"); s.State != "Retired" {
		t.Fatal("row not retired", s.State)
	}
	if _, err = controllerCall(t, d, controller.Params{Action: "rearm", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	h := awaitDrive(t, d, true, 2)
	if h.InFlight != 0 {
		t.Fatal("retired row counted in flight", h.InFlight)
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	stalls := 0
	w := controller.Watchdog{MarkerPath: d.controllerMarkerPath(), StatePath: filepath.Join(root, "state"), Message: "message", Stall: time.Nanosecond, CheckFilesystem: func(string) (bool, bool, error) { return false, false, nil },
		RPC: func(ctx context.Context, p controller.Params) (controller.Health, error) {
			if p.Action == "alert" && p.Cause == "drive-stall" {
				stalls++
				return controller.Health{}, nil
			}
			return controllerCall(t, d, p)
		}}
	if err = w.Tick(ctx, time.Now().Add(time.Hour)); err != nil || stalls != 0 {
		t.Fatal("retired row raised a stall", stalls, err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "staged", Tree: "tree", AuthorizingMessage: "message"}, "staged"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.reached:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if h = d.controllerHealth(); h.InFlight != 1 {
		t.Fatal("staged task not counted exactly once", h.InFlight)
	}
	if err = w.Tick(ctx, time.Now().Add(time.Hour)); err != nil || stalls != 1 {
		t.Fatal("staged task raised no stall", stalls, err)
	}
	// The configured budget of one re-arm per cause per hour is spent.
	if _, err = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	awaitDrive(t, d, false, 2)
	if _, err = controllerCall(t, d, controller.Params{Action: "rearm", AuthorizingMessage: "message"}); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatal("configured re-arm budget not enforced", err)
	}
}

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/daemon"
)

type fakeLand struct {
	params  daemon.LandParams
	polls   int
	results []controller.LandResult
	rolled  []controller.Options
	rollErr error
	alerts  []string
}

func (f *fakeLand) install(t *testing.T) {
	old := newLandClient
	t.Cleanup(func() { newLandClient = old })
	newLandClient = func(common) landClient {
		return landClient{
			start: func(_ context.Context, p daemon.LandParams) (daemon.LandingRecord, error) {
				f.params = p
				return daemon.LandingRecord{ID: "landing-1", State: "running", Tasks: p.Tasks, Gated: "refs/fae/gated/main"}, nil
			},
			status: func(context.Context, string) (daemon.LandingRecord, error) {
				f.polls++
				if f.polls < 2 {
					return daemon.LandingRecord{ID: "landing-1", State: "running"}, nil
				}
				return daemon.LandingRecord{ID: "landing-1", State: "finished", Tasks: f.params.Tasks, Gated: "refs/fae/gated/main", Results: f.results}, nil
			},
			alert: func(_ context.Context, cause, note string) error {
				f.alerts = append(f.alerts, cause+": "+note)
				return nil
			},
			roll: func(_ context.Context, o controller.Options) (controller.Health, error) {
				f.rolled = append(f.rolled, o)
				return controller.Health{PID: 20, OldPID: 10, Consistent: true}, f.rollErr
			},
		}
	}
}

func landArgs(extra ...string) []string {
	return append([]string{"land", "--authorizing-message", "m", "--target", "main", "--poll", "10ms"}, extra...)
}

func TestLandCommandThenRollFailureIsNonzeroAndAlerts(t *testing.T) {
	f := &fakeLand{results: []controller.LandResult{{Task: "t", Outcome: controller.LandLanded, Landed: "abc"}}, rollErr: errors.New("acceptance failed; previous binary restored")}
	f.install(t)
	var out bytes.Buffer
	e := execute(context.Background(), landArgs("--task", "t", "--then-roll", "--launcher-socket", "/private/launcher.sock"), &out)
	if exitCode(e) == 0 || !strings.Contains(e.Error(), "roll failed") {
		t.Fatal("failed roll after landing reported success", e)
	}
	if len(f.alerts) != 1 || !strings.HasPrefix(f.alerts[0], "then-roll-failed") || !strings.Contains(out.String(), "acceptance failed") {
		t.Fatal(f.alerts, out.String())
	}
	if len(f.rolled) != 1 || f.rolled[0].BuildRef != "refs/fae/gated/main" || f.rolled[0].GatedRef != "refs/fae/gated/main" || !f.rolled[0].WhenClear {
		t.Fatalf("then-roll must follow the gated reference when QA clears: %+v", f.rolled)
	}
	f.rollErr, f.polls, f.alerts = nil, 0, nil
	out.Reset()
	if e = execute(context.Background(), landArgs("--task", "t", "--then-roll", "--launcher-socket", "/private/launcher.sock"), &out); e != nil || !strings.Contains(out.String(), `"new_pid": 20`) || len(f.alerts) != 0 {
		t.Fatal(e, out.String())
	}
}

func TestLandCommandHeldBatchIsNonzeroAndNeverRolls(t *testing.T) {
	f := &fakeLand{results: []controller.LandResult{{Task: "a", Outcome: controller.LandLanded}, {Task: "b", Outcome: controller.LandHeld, FailingGate: "filter"}, {Task: "c", Outcome: controller.LandNotAttempted}}}
	f.install(t)
	var out bytes.Buffer
	e := execute(context.Background(), landArgs("--batch", "a, b,c", "--then-roll", "--launcher-socket", "/private/launcher.sock"), &out)
	if exitCode(e) != 1 || !strings.Contains(e.Error(), "b held filter") || len(f.rolled) != 0 {
		t.Fatal(e, f.rolled)
	}
	if strings.Join(f.params.Tasks, ",") != "a,b,c" || len(f.params.Flakes) != 0 || len(f.params.Frozen) != 0 || f.params.Salvage {
		t.Fatalf("%+v", f.params)
	}
	f.polls = 0
	f.results = []controller.LandResult{{Task: "s", Outcome: controller.LandLanded}}
	if e = execute(context.Background(), landArgs("--task", "s", "--salvage", "--source-ref", "tree"), &out); e != nil || !f.params.Salvage || f.params.SourceRef != "tree" {
		t.Fatal(e, f.params)
	}
	if e := execute(context.Background(), []string{"land", "--target", "main", "--task", "t"}, &out); exitCode(e) != 1 || len(f.rolled) != 0 {
		t.Fatal("uncited land must refuse with 1", e)
	}
	for _, args := range [][]string{
		// FAE-CTL-030: flakes and frozen checks are the pinned target's, not flags.
		landArgs("--task", "t", "--flakes", "TestFlaky"), landArgs("--task", "t", "--frozen", "checks.json"),
		{"land", "--task", "t"},
		landArgs(),
		landArgs("--task", "t", "--batch", "a,b"),
		landArgs("--task", "t", "--salvage"),
		landArgs("--task", "t", "--source-ref", "tree"),
		landArgs("--batch", "a,b", "--salvage", "--source-ref", "tree"),
		landArgs("--task", "t", "--then-roll"),
		{"land", "--authorizing-message", "m", "--task", "t"},
	} {
		if e := execute(context.Background(), args, &out); exitCode(e) != 2 {
			t.Fatal(args, e)
		}
	}
}

// FAE-CTL-001 step 6, FAE-CTL-033: land --then-roll rolls through the same
// controller client as fae roll, so its acceptance checks the running
// watchdog's sandbox state.
func TestThenRollUsesTheWatchdogCheckingRollClient(t *testing.T) {
	var clients []controller.Client
	old := runRoll
	t.Cleanup(func() { runRoll = old })
	runRoll = func(_ context.Context, c controller.Client, _ controller.Options) (controller.Health, error) {
		clients = append(clients, c)
		return controller.Health{}, errors.New("stop after capture")
	}
	home := t.TempDir()
	if _, e := newLandClient(common{home: home, instance: "i"}).roll(context.Background(), controller.Options{}); e == nil {
		t.Fatal("captured roll succeeded")
	}
	_ = execute(context.Background(), []string{"roll", "--home", home, "--instance", "i", "--authorizing-message", "m", "--launcher-socket", "/private/launcher.sock", "--binary", "/private/fae", "--note", "capture"}, &bytes.Buffer{})
	if len(clients) != 2 {
		t.Fatalf("roll clients %d", len(clients))
	}
	land, roll := clients[0], clients[1]
	if land.WatchdogIdentity == "" || land.WatchdogIdentity != roll.WatchdogIdentity || land.MarkerPath != roll.MarkerPath || land.RPC == nil {
		t.Fatalf("then-roll client %+v differs from roll client %+v", land, roll)
	}
}

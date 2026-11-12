package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

type gateTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *gateTestClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *gateTestClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

func gateTestHost(t *testing.T, io10 float64) (string, *bytes.Buffer) {
	t.Helper()
	oldObserve, oldClock, oldAlerts, oldProbe := gateObserve, gateClock, gateAlerts, gateProbe
	t.Cleanup(func() { gateObserve, gateClock, gateAlerts, gateProbe = oldObserve, oldClock, oldAlerts, oldProbe })
	// The default ledger and instance home live under HOME; keep them private.
	t.Setenv("HOME", t.TempDir())
	gateProbe = func(scheduler.Owner) scheduler.Liveness { return scheduler.Unknown }
	gateObserve = func(context.Context) (hostguard.Sample, error) {
		return hostguard.Sample{Memory: hostguard.Reading{Value: 32 * float64(hostguard.GiB), Known: true}, Swap: hostguard.Reading{Value: float64(hostguard.GiB), Known: true}, IO10: hostguard.Reading{Value: io10, Known: true}}, nil
	}
	gateClock = &gateTestClock{now: time.Unix(1000, 0)}
	alerts := &bytes.Buffer{}
	gateAlerts = alerts
	return filepath.Join(t.TempDir(), "host-ledger"), alerts
}

func TestGatesSelfTestCommand(t *testing.T) {
	var out bytes.Buffer
	if e := execute(context.Background(), []string{"gates", "self-test", "--offline"}, &out); e != nil {
		t.Fatal(e, out.String())
	}
	// FAE-API-010: the default form is a thin client of the GatesSelfTest read.
	served, _ := controller.SelfTest(daemon.GateSelfTestCases())
	home, requests := recordingDaemon(t, "gst", map[string]any{"passed": false, "gates": served[:1]})
	var rpcOut bytes.Buffer
	if e := execute(context.Background(), []string{"gates", "self-test", "--home", home, "--instance", "gst"}, &rpcOut); exitCode(e) != 1 || !strings.Contains(rpcOut.String(), served[0].Gate) {
		t.Fatal("daemon self-test failure not reported", e, rpcOut.String())
	}
	if r := <-requests; r.Method != "GatesSelfTest" {
		t.Fatal(r.Method)
	}
	var report struct {
		Passed bool
		Gates  []struct {
			Gate    string
			CanFail bool `json:"can_fail"`
			CanPass bool `json:"can_pass"`
		}
	}
	if e := json.Unmarshal(out.Bytes(), &report); e != nil || !report.Passed || len(report.Gates) < 11 {
		t.Fatal(report, e, out.String())
	}
	for _, g := range report.Gates {
		if !g.CanFail || !g.CanPass {
			t.Fatal(g)
		}
	}
	for _, args := range [][]string{{"gates"}, {"gates", "run"}, {"gates", "self-test", "extra"}} {
		if e := execute(context.Background(), args, &out); exitCode(e) != 2 {
			t.Fatal(args, e)
		}
	}
}

func TestGateRunReturnsCommandExitCode(t *testing.T) {
	ledger, alerts := gateTestHost(t, 1)
	var out bytes.Buffer
	if e := execute(context.Background(), []string{"gate", "run", "--host-ledger", ledger, "--", "sh", "-c", "echo gated-$LANG; exit 0"}, &out); e != nil || !strings.Contains(out.String(), "gated-C") {
		t.Fatal(e, out.String())
	}
	if e := execute(context.Background(), []string{"gate", "run", "--host-ledger", ledger, "--", "sh", "-c", "exit 9"}, &out); exitCode(e) != 9 {
		t.Fatal("command exit code not returned", e)
	}
	if alerts.Len() != 0 {
		t.Fatal("routine two-reading I/O window raised an alert", alerts.String())
	}
	l, e := scheduler.OpenDynamicLedger(ledger, 2)
	if e != nil {
		t.Fatal(e)
	}
	if pool, e := l.Snapshot(context.Background()); e != nil || len(pool.Holders)+len(pool.Waiting) != 0 {
		t.Fatal("gate run leaked its lease", pool, e)
	}
	for _, args := range [][]string{
		{"gate", "run", "--host-ledger", "", "--", "true"},
		{"gate", "run", "--host-ledger", "relative", "--", "true"},
		{"gate", "run", "--host-ledger", ledger},
		{"gate", "run", "--host-ledger", ledger, "--"},
		{"gate", "run", "--host-ledger", ledger, "--weight", "3", "--", "true"},
	} {
		if e := execute(context.Background(), args, &out); exitCode(e) != 2 {
			t.Fatal(args, e)
		}
	}
}

func TestGateRunHeldLeaseTimesOutWithDistinctCode(t *testing.T) {
	proves(t, "gate run", 75)
	proves(t, "gate run", 76)
	ledger, alerts := gateTestHost(t, 1)
	l, e := scheduler.OpenDynamicLedger(ledger, 1)
	if e != nil {
		t.Fatal(e)
	}
	if e = l.Enqueue(context.Background(), scheduler.Request{ID: "sibling-workspace-lock", Owner: scheduler.Owner{PID: 1, Started: 1}, Weight: 1, Queued: time.Now().UTC()}); e != nil {
		t.Fatal(e)
	}
	if _, _, e = l.Advance(context.Background(), time.Now().UTC(), map[scheduler.Owner]scheduler.Liveness{}); e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	e = execute(context.Background(), []string{"gate", "run", "--host-ledger", ledger, "--max-wait", "4m", "--slice", "1m", "--", "sh", "-c", "echo ran"}, &out)
	if exitCode(e) != daemon.GateRunTimeoutExit || strings.Contains(out.String(), "ran") {
		t.Fatal(exitCode(e), e, out.String())
	}
	lines := strings.Count(alerts.String(), "gate waiting stage=lease id=")
	if lines < 3 || strings.Count(alerts.String(), "holders=sibling-workspace-lock(pid=1") != lines+1 {
		t.Fatal("holder not named on every slice", alerts.String())
	}
	if daemon.GateRunTimeoutExit == daemon.GateRunRefusedExit {
		t.Fatal("distinct exit codes required")
	}
	gateTestHost(t, 1)
	gateObserve = func(context.Context) (hostguard.Sample, error) {
		return hostguard.Sample{Memory: hostguard.Reading{Value: float64(hostguard.GiB), Known: true}, Swap: hostguard.Reading{Value: 1, Known: true}}, nil
	}
	if e = execute(context.Background(), []string{"gate", "run", "--host-ledger", ledger, "--", "true"}, &out); exitCode(e) != daemon.GateRunRefusedExit {
		t.Fatal("hard breaker refusal must use the refusal code", e)
	}
}

// FAE-HOST-020, -003, -031: gate run's command scratch is on disk under the
// instance root and removed afterwards; without --host-ledger it uses the
// host-stable default ledger; a hard refusal is spooled to the instance.
func TestGateRunScratchOnDiskDefaultLedgerAndSpooledHardRefusal(t *testing.T) {
	gateTestHost(t, 1)
	home := t.TempDir()
	var out bytes.Buffer
	if e := execute(context.Background(), []string{"gate", "run", "--home", home, "--instance", "gates", "--", "sh", "-c", "echo tmp=$TMPDIR; touch $TMPDIR/scratch"}, &out); e != nil {
		t.Fatal(e, out.String())
	}
	tmp := strings.TrimSpace(strings.TrimPrefix(out.String(), "tmp="))
	if !strings.HasPrefix(tmp, filepath.Join(home, "gates", "gate-runs")+"/") {
		t.Fatal("gate run scratch not under the instance root", tmp)
	}
	if _, e := os.Stat(tmp); !os.IsNotExist(e) {
		t.Fatal("gate run scratch not removed", e)
	}
	ledger, e := scheduler.DefaultHostLedger()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(ledger, "state.json")); e != nil {
		t.Fatal("default host ledger not used", e)
	}
	layout, e := daemon.Paths(home, "gates")
	if e != nil {
		t.Fatal(e)
	}
	spool := daemon.HostAlertSpool(layout)
	if e = os.MkdirAll(spool, 0700); e != nil {
		t.Fatal(e)
	}
	gateObserve = func(context.Context) (hostguard.Sample, error) {
		return hostguard.Sample{Memory: hostguard.Reading{Value: float64(hostguard.GiB), Known: true}, Swap: hostguard.Reading{Value: 1, Known: true}}, nil
	}
	if e = execute(context.Background(), []string{"gate", "run", "--home", home, "--instance", "gates", "--", "true"}, &out); exitCode(e) != daemon.GateRunRefusedExit {
		t.Fatal(e)
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 1 {
		t.Fatal("hard refusal not spooled to the instance", entries)
	}
}

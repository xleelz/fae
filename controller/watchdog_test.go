package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatchdogMaintenanceAndDurableAlertReplay(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.Chmod(root, 0700)
	marker := filepath.Join(root, "maintenance")
	var output bytes.Buffer
	alive := false
	alerts := 0
	w := Watchdog{MarkerPath: marker, StatePath: filepath.Join(root, "watchdog.json"), Message: "auth", Output: &output, RPC: func(_ context.Context, p Params) (Health, error) {
		if !alive {
			return Health{}, errors.New("daemon absent")
		}
		if p.Action == "alert" {
			alerts++
		}
		return Health{DriveAlive: true}, nil
	}}
	now := time.Now()
	raw, _ := json.Marshal(Marker{ID: "m", Message: "auth", State: "stopped"})
	_ = os.WriteFile(marker, raw, 0600)
	if e := w.Tick(context.Background(), now); e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(output.Bytes(), []byte("daemon-dead")) {
		t.Fatal("maintenance generateddeathalert")
	}
	_ = os.Remove(marker)
	if e := w.Tick(context.Background(), now.Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(output.Bytes(), []byte("daemon-dead")) {
		t.Fatal("deathsilenced")
	}
	alive = true
	if e := w.Tick(context.Background(), now.Add(2*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if alerts != 1 {
		t.Fatal(alerts)
	}
}
func TestWatchdogRearmBoundAndAllowlistedStall(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.Chmod(root, 0700)
	rearms, stalls := 0, 0
	now := time.Now()
	w := Watchdog{MarkerPath: filepath.Join(root, "maintenance"), StatePath: filepath.Join(root, "state"), Message: "auth", RPC: func(_ context.Context, p Params) (Health, error) {
		if p.Action == "rearm" {
			rearms++
		}
		if p.Action == "alert" && p.Cause == "drive-stall" {
			stalls++
		}
		return Health{DriveAlive: false, InFlight: 0, LastTransition: now.Add(-time.Hour)}, nil
	}}
	for i := 0; i < 5; i++ {
		if e := w.Tick(context.Background(), now.Add(time.Duration(i)*time.Minute)); e != nil {
			t.Fatal(e)
		}
	}
	if rearms != 3 || stalls != 0 {
		t.Fatal(rearms, stalls)
	}
	if e := w.Tick(context.Background(), now.Add(2*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if rearms != 4 {
		t.Fatal(rearms)
	}
}

func TestFilesystemPressure(t *testing.T) {
	for _, test := range []struct {
		blocks, available, files, free uint64
		bytesLow, inodesLow            bool
	}{{0, 0, 0, 0, false, false}, {100, 20, 100, 21, true, false}, {100, 21, 100, 20, false, true}, {100, 0, 100, 0, true, true}, {100, 100, 100, 100, false, false}} {
		bytesLow, inodesLow := FilesystemPressure(test.blocks, test.available, test.files, test.free)
		if bytesLow != test.bytesLow || inodesLow != test.inodesLow {
			t.Fatal(test)
		}
	}
}
func TestWatchdogPositiveStallAndFilesystemAlerts(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.Chmod(root, 0700)
	now := time.Now()
	seen := map[string]bool{}
	w := Watchdog{MarkerPath: filepath.Join(root, "marker"), StatePath: filepath.Join(root, "state"), Message: "auth", Paths: []string{root}, CheckFilesystem: func(string) (bool, bool, error) { return true, true, nil }, RPC: func(_ context.Context, p Params) (Health, error) {
		if p.Action == "alert" {
			seen[p.Cause] = true
		}
		return Health{DriveAlive: true, InFlight: 1, LastTransition: now.Add(-time.Hour)}, nil
	}}
	if e := w.Tick(context.Background(), now); e != nil {
		t.Fatal(e)
	}
	for _, cause := range []string{"drive-stall", "filesystem-bytes:" + root, "filesystem-inodes:" + root} {
		if !seen[cause] {
			t.Fatal(cause)
		}
	}
}

func TestWatchdogPreflightFailureDoesNotSilenceDeath(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	os.Chmod(root, 0700)
	marker := filepath.Join(root, "marker")
	raw, _ := json.Marshal(Marker{ID: "failure", Message: "auth", State: "preflight-failed"})
	os.WriteFile(marker, raw, 0600)
	var output bytes.Buffer
	w := Watchdog{Message: "auth", MarkerPath: marker, StatePath: filepath.Join(root, "state"), Output: &output, RPC: func(context.Context, Params) (Health, error) {
		return Health{}, errors.New("daemon died after refusal")
	}}
	if e := w.Tick(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(output.Bytes(), []byte("daemon-dead")) {
		t.Fatal(output.String())
	}
}

func TestWatchdogRegistersBeforeMutationAndRefusesRevokedGrant(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	calls := []string{}
	w := Watchdog{MarkerPath: filepath.Join(root, "marker"), StatePath: filepath.Join(root, "state"), Message: "auth", RPC: func(_ context.Context, p Params) (Health, error) {
		calls = append(calls, p.Action)
		if p.Action == "watchdog-register" {
			return Health{}, errors.New("grant revoked")
		}
		return Health{DriveAlive: false}, nil
	}}
	if e := w.Tick(context.Background(), time.Now()); e == nil {
		t.Fatal("revoked grant ignored")
	}
	if len(calls) != 2 || calls[0] != "health" || calls[1] != "watchdog-register" {
		t.Fatal(calls)
	}
}

// FAE-CTL-020: the re-arm budget, its window and the filesystem threshold are
// configurable, and the workspace filesystem is watched.
func TestWatchdogConfiguredBoundsAndWorkspaceFilesystem(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.Chmod(root, 0700)
	workspace := filepath.Join(root, "workspace")
	rearms, checked := 0, []string{}
	now := time.Now()
	w := Watchdog{MarkerPath: filepath.Join(root, "maintenance"), StatePath: filepath.Join(root, "state"), Message: "auth", RearmBudget: 5, RearmWindow: 10 * time.Minute, Paths: []string{root},
		CheckFilesystem: func(path string) (bool, bool, error) { checked = append(checked, path); return false, false, nil },
		RPC: func(_ context.Context, p Params) (Health, error) {
			if p.Action == "rearm" {
				rearms++
			}
			return Health{DriveAlive: false, Workspace: workspace}, nil
		}}
	for i := 0; i < 7; i++ {
		if e := w.Tick(context.Background(), now.Add(time.Duration(i)*time.Minute)); e != nil {
			t.Fatal(e)
		}
	}
	if rearms != 5 {
		t.Fatal("configured budget", rearms)
	}
	if e := w.Tick(context.Background(), now.Add(11*time.Minute)); e != nil || rearms != 6 {
		t.Fatal("configured window", rearms, e)
	}
	if len(checked) < 2 || checked[0] != root || checked[1] != workspace {
		t.Fatal("workspace filesystem not watched", checked)
	}
	for _, tc := range []struct {
		used          float64
		low, inodeLow bool
	}{{0.8, true, false}, {0.9, false, false}, {0.5, true, true}} {
		if b, i := FilesystemPressureAt(100, 20, 100, 50, tc.used); b != tc.low || i != tc.inodeLow {
			t.Fatal(tc, b, i)
		}
	}
}

// bounds falls back to the default filesystem threshold for NaN and ±Inf:
// NaN fails every comparison, so it must not slip past the range check. It
// never refuses; a valid threshold passes unchanged.
func TestWatchdogBoundsFallsBackOnNonFiniteThreshold(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, _, got := (Watchdog{FilesystemThreshold: v}).bounds(); got != DefaultFilesystemThreshold {
			t.Fatalf("threshold %v: got %v, want %v", v, got, DefaultFilesystemThreshold)
		}
	}
	if _, _, got := (Watchdog{FilesystemThreshold: 0.5}).bounds(); got != 0.5 {
		t.Fatalf("threshold 0.5: got %v, want 0.5", got)
	}
}

// FAE-CTL-010, FAE-CLI-001, FAE-OBS-001: the hourly maintenance heartbeat
// carries an RFC 3339 time, the instance and the component; under --json it
// is a JSON object, so every line the watchdog writes is JSON.
func TestWatchdogHeartbeatTimestampedAndJSONUnderJSON(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.Chmod(root, 0700)
	marker := filepath.Join(root, "maintenance")
	raw, _ := json.Marshal(Marker{ID: "m", Message: "auth", State: "stopped", NextAction: "maintenance start"})
	_ = os.WriteFile(marker, raw, 0600)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, asJSON := range []bool{true, false} {
		var output bytes.Buffer
		w := Watchdog{MarkerPath: marker, StatePath: filepath.Join(root, fmt.Sprint("state-", asJSON)), Message: "auth", Output: &output, JSON: asJSON, Instance: "inst",
			Paths: []string{root}, CheckFilesystem: func(string) (bool, bool, error) { return true, false, nil },
			RPC: func(context.Context, Params) (Health, error) {
				return Health{}, errors.New("daemon stopped for maintenance")
			}}
		for i := 0; i < 3; i++ { // hourly: the second tick is within the hour
			if e := w.Tick(context.Background(), now.Add(time.Duration(i)*40*time.Minute)); e != nil {
				t.Fatal(e)
			}
		}
		lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
		beats := 0
		for _, line := range lines {
			var v map[string]any
			isJSON := json.Unmarshal([]byte(line), &v) == nil
			if asJSON && !isJSON {
				t.Fatalf("--json watchdog wrote a non-JSON line %q", line)
			}
			if asJSON && v["event"] == "maintenance-heartbeat" {
				beats++
				if v["time"] != "2026-10-24T12:00:00Z" && v["time"] != "2026-10-24T13:20:00Z" || v["instance"] != "inst" || v["component"] != "watchdog" || v["marker"] != "m" || v["state"] != "stopped" || v["next_action"] != "maintenance start" {
					t.Fatalf("heartbeat %q", line)
				}
			}
			if !asJSON && !isJSON {
				beats++
				stamp, rest, _ := strings.Cut(line, " ")
				if _, e := time.Parse(time.RFC3339, stamp); e != nil || rest != "instance=inst component=watchdog maintenance m: stopped; next: maintenance start" {
					t.Fatalf("text heartbeat %q", line)
				}
			}
		}
		if beats != 2 {
			t.Fatalf("json=%v: %d heartbeats in %q", asJSON, beats, output.String())
		}
	}
}

// A watchdog stopped mid-tick (its context cancelled during an RPC) exits
// cleanly: the cancellation is the stop request, not a failure, so a stopped
// fae watchdog exits 0 rather than 1 (FAE-CLI-001).
func TestWatchdogStoppedMidTickExitsCleanly(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.Chmod(root, 0700)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := Watchdog{MarkerPath: filepath.Join(root, "marker"), StatePath: filepath.Join(root, "state"), Message: "auth", Period: time.Hour, RPC: func(ctx context.Context, p Params) (Health, error) {
		if p.Action == "watchdog-register" {
			cancel()
			<-ctx.Done()
			return Health{}, ctx.Err()
		}
		return Health{DriveAlive: true}, nil
	}}
	if e := w.Run(ctx); e != nil {
		t.Fatal("a stop during a tick failed the watchdog:", e)
	}
}

// FAE-CTL-020: Watchdog.Run refuses a period below MinWatchdogPeriod, zero
// and negative included, before it ticks or writes anything, for every
// caller and not only the CLI. Before, a period of zero or less silently
// became one minute, and a positive period below the floor ticked back to
// back.
func TestWatchdogRunRefusesAPeriodBelowTheFloor(t *testing.T) {
	for _, period := range []time.Duration{0, -time.Minute, time.Nanosecond, 10*time.Millisecond - time.Microsecond} {
		root, _ := filepath.EvalSymlinks(t.TempDir())
		_ = os.Chmod(root, 0700)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		calls := 0
		w := Watchdog{MarkerPath: filepath.Join(root, "marker"), StatePath: filepath.Join(root, "state"), Message: "auth", Period: period, RPC: func(context.Context, Params) (Health, error) {
			calls++
			return Health{DriveAlive: true}, nil
		}}
		e := w.Run(ctx)
		cancel()
		if e == nil || !strings.Contains(e.Error(), "watchdog period must be at least 10ms") {
			t.Errorf("period %s: Run returned %v; want a refusal naming the floor", period, e)
		}
		if calls != 0 {
			t.Errorf("period %s: the refused watchdog ticked %d RPCs", period, calls)
		}
		if _, err := os.Stat(WatchdogIdentityPath(w.StatePath)); !os.IsNotExist(err) {
			t.Errorf("period %s: the refused watchdog wrote its identity: %v", period, err)
		}
	}
}

// FAE-CTL-020: when the daemon reports DriveAlive=false, the watchdog emits
// drive-exit, attempts re-arm within the configured budget, emits
// drive-rearm-failed when the rearm RPC fails, and emits drive-rearm-exhausted
// once the budget in the current window is consumed, persisting the rearm
// window and pending alerts across ticks.
func TestWatchdogDriveExitRearmFailedAndExhaustedAlerts(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.Chmod(root, 0700)
	statePath := filepath.Join(root, "state.json")
	var output bytes.Buffer
	var rpcAlerts []WatchdogAlert
	rearmCalls := 0
	failRearm := true
	failAlertRPC := false
	now := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)

	w := Watchdog{
		MarkerPath:  filepath.Join(root, "maintenance"),
		StatePath:   statePath,
		Message:     "auth",
		Output:      &output,
		RearmBudget: 2,
		RearmWindow: 30 * time.Minute,
		RPC: func(_ context.Context, p Params) (Health, error) {
			switch p.Action {
			case "health":
				return Health{DriveAlive: false, DriveCause: "panic-recovery"}, nil
			case "watchdog-register":
				return Health{}, nil
			case "rearm":
				rearmCalls++
				if failRearm {
					return Health{}, errors.New("rearm refused by daemon")
				}
				return Health{}, nil
			case "alert":
				if failAlertRPC {
					return Health{}, errors.New("alert store unavailable")
				}
				rpcAlerts = append(rpcAlerts, WatchdogAlert{Cause: p.Cause, Detail: p.Note})
				return Health{}, nil
			}
			return Health{}, errors.New("unexpected " + p.Action)
		},
	}

	readState := func() watchdogState {
		t.Helper()
		raw, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		var st watchdogState
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	// Tick 1: DriveAlive=false and rearm fails -> emits drive-exit and drive-rearm-failed.
	if err := w.Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if rearmCalls != 1 {
		t.Fatalf("rearmCalls = %d, want 1", rearmCalls)
	}
	st := readState()
	if got := st.Rearms["panic-recovery"]; got.Count != 1 || !got.Start.Equal(now) {
		t.Fatalf("state.Rearms after tick 1 = %+v", got)
	}
	if len(st.Pending) != 0 {
		t.Fatalf("expected delivered alerts cleared from Pending, got %+v", st.Pending)
	}
	wantTick1 := map[string]string{
		"drive-exit":         "drive reported stopped",
		"drive-rearm-failed": "rearm refused by daemon",
	}
	for _, a := range rpcAlerts {
		if want, ok := wantTick1[a.Cause]; !ok || a.Detail != want {
			t.Fatalf("unexpected RPC alert %+v", a)
		}
		delete(wantTick1, a.Cause)
	}
	if len(wantTick1) != 0 {
		t.Fatalf("missing RPC alerts after tick 1: %+v", wantTick1)
	}

	// Tick 2: rearm succeeds (consuming attempt 2 of 2).
	rpcAlerts = nil
	failRearm = false
	if err := w.Tick(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if rearmCalls != 2 {
		t.Fatalf("rearmCalls = %d, want 2", rearmCalls)
	}
	if len(rpcAlerts) != 1 || rpcAlerts[0].Cause != "drive-exit" || rpcAlerts[0].Detail != "drive reported stopped" {
		t.Fatalf("tick 2 RPC alerts = %+v", rpcAlerts)
	}
	if st = readState(); st.Rearms["panic-recovery"].Count != 2 {
		t.Fatalf("state.Rearms after tick 2 = %+v", st.Rearms)
	}

	// Tick 3: budget exhausted within window, and alert RPC temporarily fails ->
	// drive-exit and drive-rearm-exhausted remain pending in state.
	rpcAlerts = nil
	failAlertRPC = true
	if err := w.Tick(context.Background(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if rearmCalls != 2 {
		t.Fatalf("exhausted budget should not call rearm; rearmCalls = %d", rearmCalls)
	}
	st = readState()
	if st.Pending["drive-exit"].Detail != "drive reported stopped" || st.Pending["drive-rearm-exhausted"].Detail != "2 attempts consumed in the current 30m0s window" {
		t.Fatalf("pending alerts after failed alert RPC = %+v", st.Pending)
	}

	// Tick 4: alert RPC recovers -> drive-exit and drive-rearm-exhausted are delivered and cleared.
	failAlertRPC = false
	if err := w.Tick(context.Background(), now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	wantTick4 := map[string]string{
		"drive-exit":            "drive reported stopped",
		"drive-rearm-exhausted": "2 attempts consumed in the current 30m0s window",
	}
	for _, a := range rpcAlerts {
		if want, ok := wantTick4[a.Cause]; !ok || a.Detail != want {
			t.Fatalf("unexpected tick 4 RPC alert %+v", a)
		}
		delete(wantTick4, a.Cause)
	}
	if len(wantTick4) != 0 {
		t.Fatalf("missing tick 4 RPC alerts: %+v", wantTick4)
	}
	if st = readState(); len(st.Pending) != 0 {
		t.Fatalf("expected Pending cleared after recovery, got %+v", st.Pending)
	}

	// Verify output stream contains all three alert causes as JSON objects.
	seenOut := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var a WatchdogAlert
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Fatalf("unmarshal output line %q: %v", line, err)
		}
		seenOut[a.Cause] = true
	}
	for _, cause := range []string{"drive-exit", "drive-rearm-failed", "drive-rearm-exhausted"} {
		if !seenOut[cause] {
			t.Fatalf("missing %q in watchdog output %q", cause, output.String())
		}
	}
}

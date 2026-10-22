package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

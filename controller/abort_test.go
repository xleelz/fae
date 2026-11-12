package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAE-CTL-001 abort paths of a stop: a drain past its bound, a refused stop
// and a daemon still alive past the stop bound each alert loudly through the
// abort RPC naming the state and the next action, and fail; none force-kills.
func TestStopAbortsLoudlyOnDrainTimeoutStopFailureAndStopTimeout(t *testing.T) {
	for _, tc := range []struct {
		state          string
		inFlight       int
		stopErr        error
		aliveAfterStop bool
	}{
		{"drain-timeout", 1, nil, false},
		{"stop-failed", 0, errors.New("graceful stop refused"), false},
		{"stop-timeout", 0, nil, true},
	} {
		t.Run(tc.state, func(t *testing.T) {
			marker := &Marker{ID: "m", Message: "auth", OldPID: os.Getpid(), OldBirth: "live"}
			aborts, stopped := []string{}, false
			c := Client{Poll: time.Millisecond, RPC: func(_ context.Context, p Params) (Health, error) {
				switch p.Action {
				case "prepare":
					return Health{Maintenance: marker}, nil
				case "health":
					if stopped && !tc.aliveAfterStop {
						return Health{}, errors.New("gone")
					}
					return Health{InFlight: tc.inFlight}, nil
				case "stop":
					stopped = tc.stopErr == nil
					return Health{}, tc.stopErr
				case "abort":
					aborts = append(aborts, p.Note)
					return Health{}, nil
				}
				return Health{}, errors.New("unexpected " + p.Action)
			}}
			_, err := c.Stop(context.Background(), Options{Message: "auth", Note: "planned", Launcher: "/tmp/launcher", MaxWait: 20 * time.Millisecond, OverlapWait: time.Millisecond})
			if err == nil {
				t.Fatal("abort path succeeded")
			}
			if len(aborts) != 1 || !strings.HasPrefix(aborts[0], tc.state+": ") || len(strings.Split(aborts[0], ": ")) < 3 {
				t.Fatalf("abort not alerted with state and next action: %q", aborts)
			}
		})
	}
}

// FAE-CTL-001: a start that fails leaves an explicit marker naming the failed
// state and the next action, never a silently down instance.
func TestStartFailureLeavesAnExplicitMarker(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "fae-start-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	dir, _ = filepath.EvalSymlinks(dir)
	path := filepath.Join(dir, "maintenance.json")
	exe, _ := os.Executable()
	hash, _ := HashFile(exe)
	marker := Marker{ID: "m", Message: "auth", State: "stopped", NextAction: "maintenance start through authorized runtime launcher", OldPID: 99999999, OldBirth: "gone", Launcher: filepath.Join(dir, "absent.sock"), Launch: LaunchSpec{Binary: exe, Directory: dir, Environment: []string{"A=1"}, SHA256: hash}}
	raw, _ := json.Marshal(marker)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	c := Client{MarkerPath: path, Poll: time.Millisecond, RPC: func(context.Context, Params) (Health, error) { return Health{}, errors.New("daemon down") }}
	if _, err = c.Start(context.Background(), Options{Message: "auth", MaxWait: time.Second}); err == nil || !strings.Contains(err.Error(), "start failed") {
		t.Fatal("start failure not reported", err)
	}
	after, err := ReadMarker(path)
	if err != nil || after == nil || after.State != "failed" || !strings.Contains(after.NextAction, "start failed") || !strings.Contains(after.NextAction, "retry fae maintenance start") {
		t.Fatalf("marker after a failed start: %+v %v", after, err)
	}
	// The failed marker still authorizes a retry of the same start.
	if _, err = c.Start(context.Background(), Options{Message: "auth", MaxWait: time.Second}); err == nil || strings.Contains(err.Error(), "not proven stopped") || !strings.Contains(err.Error(), "start failed") {
		t.Fatal("retry after a failed start", err)
	}
}

// FAE-CTL-001 outputs: a roll reports the replaced binary and both versions.
func TestRollReportsOldAndNewVersions(t *testing.T) {
	f, ctx := newRollFixture(t)
	c := Client{MarkerPath: f.markerPath, Poll: time.Millisecond, RPC: func(ctx context.Context, p Params) (Health, error) {
		f.record(p.Action)
		switch p.Action {
		case "prepare":
			return Health{Maintenance: &f.marker}, nil
		case "stop":
			return Health{}, nil
		case "health":
			if strings.Count(strings.Join(f.calls, ","), "stop") == 0 {
				return Health{}, nil
			}
			return f.child()
		case "accept":
			return f.child()
		}
		return Health{}, errors.New("unexpected " + p.Action)
	}}
	h, err := c.Roll(ctx, Options{Message: "auth", Note: "deploy", Launcher: f.socket, Binary: f.replacement, MaxWait: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if h.OldPID != f.marker.OldPID || h.OldBinaryHash != f.marker.Launch.SHA256 || h.OldVersion == "" || h.NewVersion == "" || h.PID <= 1 {
		t.Fatalf("roll output %+v", h)
	}
}

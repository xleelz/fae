package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/processowner"
)

// rollFixture is a stopped instance whose replacement and previous binaries
// both start the controller helper through the real unsandboxed launcher.
type rollFixture struct {
	socket, markerPath, pidPath, replacement string
	marker                                   Marker
	env                                      []string
	hash                                     string
	mu                                       sync.Mutex
	calls                                    []string
}

func newRollFixture(t *testing.T) (*rollFixture, context.Context) {
	t.Helper()
	socket, ctx := launcherFixture(t)
	dir := filepath.Dir(socket)
	exe, _ := os.Executable()
	hash, _ := HashFile(exe)
	replacement := filepath.Join(dir, "fae-next")
	src, e := os.Open(exe)
	if e != nil {
		t.Fatal(e)
	}
	dst, e := os.OpenFile(replacement, os.O_CREATE|os.O_WRONLY, 0700)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = io.Copy(dst, src); e != nil {
		t.Fatal(e)
	}
	src.Close()
	dst.Close()
	f := &rollFixture{socket: socket, markerPath: filepath.Join(dir, "maintenance.json"), pidPath: filepath.Join(dir, "child.pid"), replacement: replacement, hash: hash}
	f.env = append(os.Environ(), "CONTROLLER_TEST_HELPER=1", "CONTROLLER_TEST_PID="+f.pidPath)
	launch := LaunchSpec{Binary: exe, Arguments: []string{"-test.run=^TestControllerHelper$"}, Directory: dir, Environment: f.env, SHA256: hash}
	next := launch
	next.Binary = replacement
	f.marker = Marker{ID: "m", Message: "auth", State: "stopped", OldPID: 99999999, OldBirth: "gone", Launcher: socket, Launch: launch, Replacement: &next, Holds: []OverlapHold{{Task: "held", Since: time.Unix(10, 0).UTC(), Critical: true}}}
	raw, _ := json.Marshal(f.marker)
	if e = os.WriteFile(f.markerPath, raw, 0600); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if b, e := os.ReadFile(f.pidPath); e == nil {
			pid, _ := strconv.Atoi(string(b))
			killControllerChild(t, pid)
		}
	})
	return f, ctx
}
func (f *rollFixture) record(action string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, action)
	n := 0
	for _, c := range f.calls {
		if c == action {
			n++
		}
	}
	return n
}
func (f *rollFixture) child() (Health, error) {
	b, e := os.ReadFile(f.pidPath)
	if e != nil {
		return Health{}, e
	}
	pid, _ := strconv.Atoi(string(b))
	birth, e := processowner.Birth(pid)
	if e != nil {
		return Health{}, e
	}
	return Health{PID: pid, DoltPID: pid, ProcessBirth: birth, BinaryHash: f.hash, EnvironmentHash: EnvironmentHash(f.env), DriveAlive: true, DriveRounds: 1, Consistent: true}, nil
}

func TestRollRejectedReplacementRollsBackToPreviousBinary(t *testing.T) {
	f, ctx := newRollFixture(t)
	notes := []string{}
	var replacementPID int
	c := Client{MarkerPath: f.markerPath, Poll: time.Millisecond, RPC: func(ctx context.Context, p Params) (Health, error) {
		n := f.record(p.Action)
		switch p.Action {
		case "prepare":
			return Health{Maintenance: &f.marker}, nil
		case "stop":
			if n == 2 {
				// The rollback gracefully stops the rejected replacement.
				h, e := f.child()
				if e == nil {
					replacementPID = h.PID
					killControllerChild(t, h.PID)
				}
			}
			return Health{}, nil
		case "health":
			if strings.Count(strings.Join(f.calls, ","), "stop") == 0 {
				return Health{}, nil
			}
			return f.child()
		case "accept":
			if n == 1 {
				return Health{}, errors.New("post-start acceptance incomplete")
			}
			return f.child()
		case "abort", "preflight-failed":
			notes = append(notes, p.Action+": "+p.Note)
			return Health{}, nil
		}
		return Health{}, errors.New("unexpected " + p.Action)
	}}
	restored, e := c.Roll(ctx, Options{Message: "auth", Note: "deploy", Launcher: f.socket, Binary: f.replacement, MaxWait: 3 * time.Second})
	if e == nil || !strings.Contains(e.Error(), "previous binary restored") {
		t.Fatal("rejected replacement did not roll back", e)
	}
	if restored.PID <= 1 || replacementPID == 0 || restored.PID == replacementPID {
		t.Fatal("rollback did not restart a distinct previous-binary daemon", restored.PID, replacementPID)
	}
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "preflight-failed: deployment failed; previous binary restored") {
		t.Fatal("rollback not alerted through an explicit failure marker", notes)
	}
}

func TestStartAbortsAcceptanceTimeoutAndChecksWatchdogSandbox(t *testing.T) {
	f, ctx := newRollFixture(t)
	notes := []string{}
	c := Client{MarkerPath: f.markerPath, Poll: time.Millisecond, RPC: func(ctx context.Context, p Params) (Health, error) {
		f.record(p.Action)
		if p.Action == "abort" {
			notes = append(notes, p.Note)
			return Health{}, nil
		}
		return Health{}, errors.New("daemon never became healthy")
	}}
	if _, e := c.Start(ctx, Options{Message: "auth", MaxWait: 300 * time.Millisecond}); e == nil || len(notes) != 1 || !strings.HasPrefix(notes[0], "acceptance-timeout: restore previous binary") {
		t.Fatal("acceptance timeout did not abort loudly", e, notes)
	}
	if b, e := os.ReadFile(f.pidPath); e == nil {
		pid, _ := strconv.Atoi(string(b))
		killControllerChild(t, pid)
		os.Remove(f.pidPath)
	}
	// A running watchdog is part of post-start sandbox acceptance.
	identity := filepath.Join(filepath.Dir(f.socket), "watchdog-identity.json")
	if e := WriteWatchdogIdentity(identity); e != nil {
		t.Fatal(e)
	}
	accepting := Client{MarkerPath: f.markerPath, WatchdogIdentity: identity, Poll: time.Millisecond, RPC: func(ctx context.Context, p Params) (Health, error) {
		if p.Action == "abort" {
			notes = append(notes, p.Note)
			return Health{}, nil
		}
		return f.child()
	}}
	h, e := accepting.Start(ctx, Options{Message: "auth", MaxWait: 3 * time.Second})
	if e != nil || h.Watchdog != "unsandboxed pid "+strconv.Itoa(os.Getpid()) {
		t.Fatal("unsandboxed watchdog not recorded in acceptance", h.Watchdog, e)
	}
	killControllerChild(t, h.PID)
	os.Remove(f.pidPath)
	old := sandboxProbe
	t.Cleanup(func() { sandboxProbe = old })
	sandboxProbe = func(pid int) (bool, error) { return pid != os.Getpid(), nil }
	notes = nil
	if h, e = accepting.Start(ctx, Options{Message: "auth", MaxWait: 3 * time.Second}); e == nil || len(notes) != 1 || !strings.HasPrefix(notes[0], "acceptance-failed") {
		t.Fatal("sandboxed watchdog accepted", e, notes)
	}
	if h.PID > 1 {
		killControllerChild(t, h.PID)
	}
	os.Remove(f.pidPath)
	sandboxProbe = old
	stale, _ := json.Marshal(map[string]any{"pid": 99999999, "birth": "gone"})
	os.WriteFile(identity, stale, 0600)
	if h, e = accepting.Start(ctx, Options{Message: "auth", MaxWait: 3 * time.Second}); e != nil || h.Watchdog != "not running" {
		t.Fatal("stopped watchdog misreported", h.Watchdog, e)
	}
}

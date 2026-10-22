package controller

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/processowner"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestControllerHelper(t *testing.T) {
	if os.Getenv("FAE_CONTROLLER_TEST_HELPER") != "1" {
		return
	}
	_ = os.WriteFile(os.Getenv("FAE_CONTROLLER_TEST_PID"), []byte(strconv.Itoa(os.Getpid())), 0600)
	for {
		time.Sleep(time.Second)
	}
}
func TestCanEnterMaintenance(t *testing.T) {
	for mask := 0; mask < 32; mask++ {
		a, b, c, d, e := mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0, mask&16 != 0
		if CanEnterMaintenance(a, b, c, d, e) != (a && (!b || c) && (d || e)) {
			t.Fatal(mask)
		}
	}
}
func launcherFixture(t *testing.T) (string, context.Context) {
	t.Helper()
	safe, e := Unsandboxed(os.Getpid())
	if e != nil || !safe {
		t.Fatalf("native unsandboxed acceptance: %v safe=%v", e, safe)
	}
	root, e := os.MkdirTemp("/private/tmp", "fae-ctl-")
	if e != nil {
		root, e = os.MkdirTemp("/tmp", "fae-ctl-")
	}
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, _ = filepath.EvalSymlinks(root)
	socket := filepath.Join(root, "launcher.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeLauncher(ctx, socket) }()
	t.Cleanup(func() {
		cancel()
		if e := <-done; e != nil {
			t.Error(e)
		}
	})
	deadline := time.Now().Add(time.Second)
	for {
		if _, e = os.Stat(socket); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("launcher startup")
		}
		time.Sleep(time.Millisecond)
	}
	return socket, ctx
}
func TestRuntimeLauncherChecksPinnedHashAndActualChild(t *testing.T) {
	socket, ctx := launcherFixture(t)
	hash, e := HashFile("/bin/sleep")
	if e != nil {
		t.Fatal(e)
	}
	spec := LaunchSpec{Binary: "/bin/sleep", Arguments: []string{"30"}, Directory: filepath.Dir(socket), Environment: os.Environ(), SHA256: hash}
	bad := spec
	bad.SHA256 = "wrong"
	if _, e := Launch(ctx, socket, bad); e == nil {
		t.Fatal("changedbinary accepted")
	}
	pid, e := Launch(ctx, socket, spec)
	if e != nil {
		t.Fatal(e)
	}
	defer killControllerChild(t, pid)
	safe, e := Unsandboxed(pid)
	if e != nil || !safe {
		t.Fatalf("child sandbox: %v %v", safe, e)
	}
}
func TestMaintenanceWaitsForDrainAndExactDeath(t *testing.T) {
	marker := &Marker{ID: "m", Message: "auth", OldPID: 99999999, OldBirth: "absent", Launch: LaunchSpec{}}
	calls := []string{}
	health := 0
	c := Client{Poll: time.Millisecond, RPC: func(ctx context.Context, p Params) (Health, error) {
		calls = append(calls, p.Action)
		switch p.Action {
		case "prepare":
			return Health{Maintenance: marker}, nil
		case "health":
			health++
			if health == 1 {
				return Health{InFlight: 1}, nil
			}
			if health == 2 {
				return Health{}, nil
			}
			return Health{}, errors.New("gone")
		case "stop":
			return Health{}, nil
		}
		return Health{}, errors.New("unexpected")
	}}
	got, e := c.Stop(context.Background(), Options{Message: "auth", Note: "planned", Launcher: "/tmp/launcher", MaxWait: time.Second})
	if e != nil || got != marker {
		t.Fatal(e)
	}
	if len(calls) != 5 || calls[3] != "stop" {
		t.Fatal(calls)
	}
}
func TestMaintenanceCommitWindowBound(t *testing.T) {
	c := Client{Poll: time.Millisecond, RPC: func(context.Context, Params) (Health, error) { return Health{Unsafe: []string{"task"}}, nil }}
	if _, e := c.Stop(context.Background(), Options{Message: "auth", Note: "planned", Launcher: "/tmp/launcher", MaxWait: 10 * time.Millisecond}); e == nil {
		t.Fatal("unsafe window ignored")
	}
}
func TestStartAcceptsOnlyLaunchedPinnedProcess(t *testing.T) {
	for _, scenario := range []string{"accepted", "identity-mismatch", "accept-rejected"} {
		mismatch := scenario == "identity-mismatch"
		t.Run(scenario, func(t *testing.T) {
			socket, ctx := launcherFixture(t)
			exe, _ := os.Executable()
			hash, _ := HashFile(exe)
			pidPath := filepath.Join(filepath.Dir(socket), "child.pid")
			env := append(os.Environ(), "FAE_CONTROLLER_TEST_HELPER=1", "FAE_CONTROLLER_TEST_PID="+pidPath)
			marker := Marker{ID: "m", Message: "auth", State: "stopped", OldPID: 99999999, OldBirth: "gone", Launcher: socket, Launch: LaunchSpec{Binary: exe, Arguments: []string{"-test.run=^TestControllerHelper$"}, Directory: filepath.Dir(socket), Environment: env, SHA256: hash}}
			markerPath := filepath.Join(filepath.Dir(socket), "maintenance.json")
			raw, _ := json.Marshal(marker)
			if e := os.WriteFile(markerPath, raw, 0600); e != nil {
				t.Fatal(e)
			}
			var accepted atomic.Bool
			c := Client{MarkerPath: markerPath, Poll: time.Millisecond, RPC: func(ctx context.Context, p Params) (Health, error) {
				b, e := os.ReadFile(pidPath)
				if e != nil {
					return Health{}, e
				}
				pid, _ := strconv.Atoi(string(b))
				birth, e := processowner.Birth(pid)
				if e != nil {
					return Health{}, e
				}
				if mismatch {
					pid = os.Getpid()
					birth, _ = processowner.Birth(pid)
				}
				h := Health{PID: pid, DoltPID: pid, ProcessBirth: birth, BinaryHash: hash, EnvironmentHash: EnvironmentHash(env), DriveAlive: true, DriveRounds: 1, Consistent: true}
				if p.Action == "accept" {
					accepted.Store(true)
					if scenario == "accept-rejected" {
						return Health{}, errors.New("integration diverged")
					}
				}
				return h, nil
			}}
			started, e := c.Start(ctx, Options{Message: "auth", MaxWait: 200 * time.Millisecond})
			if scenario == "accept-rejected" && (e == nil || started.PID <= 1 || started.ProcessBirth == "") {
				t.Fatal("lost replacement identity needed for rollback", started, e)
			}
			if b, re := os.ReadFile(pidPath); re == nil {
				pid, _ := strconv.Atoi(string(b))
				killControllerChild(t, pid)
			}
			if mismatch && (e == nil || accepted.Load()) {
				t.Fatal("acceptedoldprocess")
			}
			if scenario == "accepted" && (e != nil || !accepted.Load()) {
				t.Fatal(e)
			}
			if _, e = c.Start(ctx, Options{Message: "other", MaxWait: time.Millisecond}); e == nil {
				t.Fatal("offlineauthoritychanged")
			}
			if _, e = c.Start(ctx, Options{Message: "auth", Binary: "/bin/echo"}); e == nil {
				t.Fatal("unapprovedbinaryaccepted")
			}
		})
	}
}

func killControllerChild(t *testing.T, pid int) {
	t.Helper()
	birth, e := processowner.Birth(pid)
	if e != nil {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	until := time.Now().Add(5 * time.Second)
	for !processowner.ProvedDead(pid, birth) {
		if time.Now().After(until) {
			t.Errorf("controller helper %d not reaped", pid)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

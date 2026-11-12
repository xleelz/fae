package controller

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/processowner"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestControllerHelper(t *testing.T) {
	if os.Getenv("CONTROLLER_TEST_HELPER") != "1" {
		return
	}
	pidPath := os.Getenv("CONTROLLER_TEST_PID")
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0600)
	// A relaunch overwrites the pid file, so the fixture can lose track of an
	// earlier helper: each one exits once its fixture directory is removed.
	for {
		time.Sleep(time.Second)
		if _, err := os.Stat(filepath.Dir(pidPath)); err != nil {
			os.Exit(0)
		}
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
		root, e = os.MkdirTemp("/var/tmp", "fae-ctl-")
	}
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, _ = filepath.EvalSymlinks(root)
	socket := filepath.Join(root, "launcher.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeLauncher(ctx, socket, filepath.Join(root, "maintenance.json")) }()
	t.Cleanup(func() {
		cancel()
		if e := <-done; e != nil {
			t.Error(e)
		}
	})
	deadline := time.Now().Add(time.Second)
	for {
		if _, e = os.Stat(LauncherKeyPath(socket)); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("launcher startup")
		}
		time.Sleep(time.Millisecond)
	}
	return socket, ctx
}

// FAE-SEC-001: socket reachability grants nothing. A request without the
// launcher's key, or with a wrong one, is refused; with the key, only a launch
// the maintenance marker authorized for this launcher in an offline-start state
// runs, and the binary is hashed against the marker's digest, never against a
// digest the caller supplies.
func TestRuntimeLauncherAuthenticatesAndRunsOnlyAuthorizedLaunches(t *testing.T) {
	socket, ctx := launcherFixture(t)
	dir := filepath.Dir(socket)
	binary := filepath.Join(dir, "sleeper")
	raw, e := os.ReadFile("/bin/sleep")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(binary, raw, 0700); e != nil {
		t.Fatal(e)
	}
	hash, e := HashFile(binary)
	if e != nil {
		t.Fatal(e)
	}
	spec := LaunchSpec{Binary: binary, Arguments: []string{"30"}, Directory: dir, Environment: os.Environ(), SHA256: hash}
	writeMarker := func(m Marker) {
		t.Helper()
		b, _ := json.Marshal(m)
		if e := os.WriteFile(filepath.Join(dir, "maintenance.json"), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	authorized := Marker{ID: "m", Message: "auth", State: "stopped", Launcher: socket, Launch: spec}
	writeMarker(authorized)
	if info, e := os.Lstat(LauncherKeyPath(socket)); e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("launcher key not owner-only", info, e)
	}
	key, e := os.ReadFile(LauncherKeyPath(socket))
	if e != nil || len(key) != 64 {
		t.Fatal("launcher key is not 32 random bytes", len(key), e)
	}
	send := func(token string, s LaunchSpec) launchResponse {
		t.Helper()
		conn, e := net.Dial("unix", socket)
		if e != nil {
			t.Fatal(e)
		}
		defer conn.Close()
		var r launchResponse
		if e = json.NewEncoder(conn).Encode(launchRequest{Token: token, Spec: s}); e == nil {
			e = json.NewDecoder(conn).Decode(&r)
		}
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	wrong := strings.Repeat("0", 64)
	for name, token := range map[string]string{"without key": "", "wrong key": wrong, "key prefix": string(key[:32])} {
		if r := send(token, spec); r.PID != 0 || !strings.Contains(r.Error, "unauthenticated") {
			t.Fatal(name, "accepted", r)
		}
	}
	echo, _ := HashFile("/bin/echo")
	unauthorized := LaunchSpec{Binary: "/bin/echo", Arguments: []string{"x"}, Directory: dir, Environment: os.Environ(), SHA256: echo}
	other := spec
	other.Arguments = []string{"31"}
	forged := spec
	forged.SHA256 = "wrong"
	for name, s := range map[string]LaunchSpec{"unauthorized binary with its own hash": unauthorized, "changed arguments": other, "caller digest": forged} {
		if _, e := Launch(ctx, socket, s); e == nil || !strings.Contains(e.Error(), "not authorized") {
			t.Fatal(name, "launched", e)
		}
	}
	elsewhere := authorized
	elsewhere.Launcher = filepath.Join(dir, "other.sock")
	draining := authorized
	draining.State = "draining"
	for name, m := range map[string]Marker{"marker for another launcher": elsewhere, "daemon not stopped": draining} {
		writeMarker(m)
		if _, e := Launch(ctx, socket, spec); e == nil || !strings.Contains(e.Error(), "no authorized offline start") {
			t.Fatal(name, "launched", e)
		}
	}
	writeMarker(authorized)
	pid, e := Launch(ctx, socket, spec)
	if e != nil {
		t.Fatal(e)
	}
	defer killControllerChild(t, pid)
	safe, e := Unsandboxed(pid)
	if e != nil || !safe {
		t.Fatalf("child sandbox: %v %v", safe, e)
	}
	// Replace the authorized file (the running copy keeps its inode).
	if e = os.WriteFile(binary+".new", []byte("#!/bin/sh\nexit 0\n"), 0700); e != nil || os.Rename(binary+".new", binary) != nil {
		t.Fatal(e)
	}
	if _, e = Launch(ctx, socket, spec); e == nil || !strings.Contains(e.Error(), "hash changed") {
		t.Fatal("binary replaced after authorization launched", e)
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
			env := append(os.Environ(), "CONTROLLER_TEST_HELPER=1", "CONTROLLER_TEST_PID="+pidPath)
			marker := Marker{ID: "m", Message: "auth", State: "stopped", OldPID: 99999999, OldBirth: "gone", Launcher: socket, Launch: LaunchSpec{Binary: exe, Arguments: []string{"-test.run=^TestControllerHelper$"}, Directory: filepath.Dir(socket), Environment: env, SHA256: hash}}
			if e := SealEnvironment(filepath.Dir(socket), &marker.Launch); e != nil {
				t.Fatal(e)
			}
			markerPath := filepath.Join(filepath.Dir(socket), "maintenance.json")
			raw, _ := json.Marshal(marker)
			if strings.Contains(string(raw), pidPath) || marker.Launch.EnvironmentDigest != EnvironmentHash(env) {
				t.Fatal("launch environment values recorded in marker", string(raw))
			}
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
					if p.Marker == nil || p.Marker.ID != "m" || len(p.Marker.Launch.Environment) > 0 {
						return Health{}, errors.New("acceptance must cite only the maintenance identity")
					}
					accepted.Store(true)
					if scenario == "accept-rejected" {
						return Health{}, errors.New("integration diverged")
					}
				}
				return h, nil
			}}
			// The bound covers starting the race-built child under the full
			// suite's load; an accepting scenario returns once it answers.
			started, e := c.Start(ctx, Options{Message: "auth", MaxWait: 5 * time.Second})
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

func TestLaunchEnvironmentSealedOutsideMarker(t *testing.T) {
	dir := t.TempDir()
	env := []string{"PATH=/usr/bin", "FAE_PLANTED_TOKEN=sealed-secret"}
	spec := LaunchSpec{Binary: "/bin/true", Environment: append([]string(nil), env...)}
	if e := SealEnvironment(dir, &spec); e != nil || spec.Environment != nil || spec.EnvironmentDigest != EnvironmentHash(env) || spec.Digest() != EnvironmentHash(env) {
		t.Fatal(spec, e)
	}
	raw, _ := json.Marshal(spec)
	if strings.Contains(string(raw), "sealed-secret") || strings.Contains(string(raw), `"environment":`) {
		t.Fatal(string(raw))
	}
	path := filepath.Join(dir, "controller-environment-"+spec.EnvironmentDigest+".json")
	info, e := os.Stat(path)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, e)
	}
	again := LaunchSpec{Environment: append([]string(nil), env...)}
	if e = SealEnvironment(dir, &again); e != nil || again.EnvironmentDigest != spec.EnvironmentDigest {
		t.Fatal("resealing identical environment", e)
	}
	resolved, e := ResolveEnvironment(dir, spec)
	if e != nil || strings.Join(resolved.Environment, "\n") != strings.Join(env, "\n") || resolved.Binary != spec.Binary {
		t.Fatal(resolved, e)
	}
	legacy := LaunchSpec{Environment: env}
	if resolved, e = ResolveEnvironment(dir, legacy); e != nil || len(resolved.Environment) != 2 || legacy.Digest() != spec.EnvironmentDigest {
		t.Fatal("inline environment recorded before sealing", e)
	}
	for _, bad := range []LaunchSpec{{}, {EnvironmentDigest: "../../etc/passwd"}, {EnvironmentDigest: EnvironmentHash([]string{"OTHER=1"})}} {
		if _, e = ResolveEnvironment(dir, bad); e == nil {
			t.Fatal("resolved", bad)
		}
	}
	if e = os.Chmod(path, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e = ResolveEnvironment(dir, spec); e == nil {
		t.Fatal("readable launch environment accepted")
	}
	if e = os.WriteFile(path, []byte(`["PATH=/tampered"]`), 0600); e != nil || os.Chmod(path, 0600) != nil {
		t.Fatal(e)
	}
	if _, e = ResolveEnvironment(dir, spec); e == nil {
		t.Fatal("tampered launch environment accepted")
	}
	other := LaunchSpec{Environment: []string{"KEEP=1"}}
	if e = SealEnvironment(dir, &other); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, ".controller-environment-123"), []byte("partial"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = DiscardEnvironments(dir, other.EnvironmentDigest); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("consumed environment retained", e)
	}
	if _, e = os.Stat(filepath.Join(dir, ".controller-environment-123")); !os.IsNotExist(e) {
		t.Fatal("interrupted seal retained", e)
	}
	if _, e = ResolveEnvironment(dir, other); e != nil {
		t.Fatal("referenced environment discarded", e)
	}
}

// FAE-CTL-001: when a roll's replacement binary fails post-start acceptance
// and the rollback start also fails, Roll records an explicit
// roll-rollback-failed abort alert before returning the joined error.
func TestRollDoubleFailureRecordsAlertAndReturnsJoinedError(t *testing.T) {
	f, ctx := newRollFixture(t)
	newErr := errors.New("replacement post-start acceptance incomplete")
	rollbackErr := errors.New("rollback post-start acceptance incomplete")
	var aborts []string
	c := Client{MarkerPath: f.markerPath, Poll: time.Millisecond, RPC: func(ctx context.Context, p Params) (Health, error) {
		n := f.record(p.Action)
		switch p.Action {
		case "prepare":
			return Health{Maintenance: &f.marker}, nil
		case "stop":
			if n == 2 {
				if h, e := f.child(); e == nil {
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
				return Health{}, newErr
			}
			return Health{}, rollbackErr
		case "abort":
			aborts = append(aborts, p.Note)
			return Health{}, nil
		}
		return Health{}, errors.New("unexpected " + p.Action)
	}}
	_, err := c.Roll(ctx, Options{Message: "auth", Note: "deploy", Launcher: f.socket, Binary: f.replacement, MaxWait: 3 * time.Second})
	if !errors.Is(err, newErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("Roll error = %v, want joined %v and %v", err, newErr, rollbackErr)
	}
	if len(aborts) != 1 || !strings.HasPrefix(aborts[0], "roll-rollback-failed: restore previous binary: ") || !strings.Contains(aborts[0], newErr.Error()) || !strings.Contains(aborts[0], rollbackErr.Error()) {
		t.Fatalf("expected roll-rollback-failed abort alert, got %q", aborts)
	}
}

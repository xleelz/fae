//go:build linux

package commandshim

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/scheduler"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Unix socket names are limited by sockaddr_un, independently of the filesystem
// path limit. A broker's long TMPDIR plus t.TempDir's test name can exceed it.
func proxyTestSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "fae-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "lease.sock")
}

func TestNamespaceLeaseProxyRunsOneAccountedCommand(t *testing.T) {
	f := roleShims(t, RoleShims{Build: true, Test: true, Format: true, Local: true})
	policyPath := filepath.Join(f.shims, ".policy.json")
	policy, err := ReadRoleShims(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	policy.LeaseProxy = proxyTestSocket(t)
	if err = os.Chmod(policyPath, 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(policy)
	if err = os.WriteFile(policyPath, raw, 0400); err != nil {
		t.Fatal(err)
	}
	proxy, err := StartLeaseProxy(context.Background(), policy.LeaseProxy, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	proxy.observe, proxy.hostPolicy = healthyHost, fastPolicy
	proxy.allowHostNamespace.Store(true)
	proxy.BindProcess(os.Getpid())
	code, out := f.run(t, f.worktree, nil, filepath.Join(f.shims, "go"), "build", ".")
	if code != 5 {
		t.Fatalf("proxy command exit=%d output=%s", code, out)
	}
	if got := strings.Join(f.audit(t), ","); got != "queued,granted,released" {
		t.Fatalf("proxy double-leased or retained lease: %s", got)
	}
}

func proxyFixture(t *testing.T, grant RoleShims, script string) (roleFixture, *LeaseProxy) {
	t.Helper()
	f := roleShims(t, grant)
	path := filepath.Join(f.shims, ".policy.json")
	p, err := ReadRoleShims(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p.Tools["go"], []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	p.LeaseProxy = proxyTestSocket(t)
	raw, _ := json.Marshal(p)
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0400); err != nil {
		t.Fatal(err)
	}
	proxy, err := StartLeaseProxy(context.Background(), p.LeaseProxy, p)
	if err != nil {
		t.Fatal(err)
	}
	proxy.observe, proxy.hostPolicy = healthyHost, fastPolicy
	proxy.allowHostNamespace.Store(true)
	proxy.BindProcess(os.Getpid())
	t.Cleanup(func() { proxy.Close() })
	return f, proxy
}
func TestNamespaceLeaseProxyConcurrentCommandsRespectCapacity(t *testing.T) {
	f, _ := proxyFixture(t, RoleShims{Build: true}, "#!/bin/sh\nsleep 0.15\nexit 0\n")
	var commands []*exec.Cmd
	for range 2 {
		cmd := exec.Command(filepath.Join(f.shims, "go"), "build", "./...")
		cmd.Dir, cmd.Env = f.worktree, f.env
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	ledger, err := scheduler.OpenDynamicLedger(f.ledger, 2)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ledger.Audit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	held, maxHeld := 0, 0
	for _, row := range rows {
		switch row.Action {
		case "granted":
			held++
			if held > maxHeld {
				maxHeld = held
			}
		case "released":
			held--
		}
	}
	if maxHeld != 1 || held != 0 {
		t.Fatalf("two weight2 commands exceeded capacity2: max=%d final=%d", maxHeld, held)
	}
	if len(f.audit(t)) != 6 {
		t.Fatal("commands did not receive separate single leases")
	}
}
func TestNamespaceLeaseProxyForbiddenRoleNeverQueues(t *testing.T) {
	f, _ := proxyFixture(t, RoleShims{Test: true}, "#!/bin/sh\nexit 0\n")
	code, _ := f.run(t, f.worktree, nil, filepath.Join(f.shims, "go"), "build", ".")
	if code != Refused {
		t.Fatalf("build without role grant exit=%d", code)
	}
	if len(f.audit(t)) != 0 {
		t.Fatal("forbidden command touched host lease")
	}
}
func TestNamespaceLeaseProxyCancellationRetiresCommandBeforeRelease(t *testing.T) {
	f, proxy := proxyFixture(t, RoleShims{Build: true}, "#!/bin/sh\nsleep 60\n")
	cmd := exec.Command(filepath.Join(f.shims, "go"), "build", ".")
	cmd.Dir, cmd.Env = f.worktree, f.env
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ledger, err := scheduler.OpenDynamicLedger(f.ledger, 2)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		pool, e := ledger.Snapshot(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		if len(pool.Holders) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command never granted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("cancelled heavy command succeeded")
	}
	if got := strings.Join(f.audit(t), ","); got != "queued,granted,released" {
		t.Fatal("lease not released after cancelled command", got)
	}
}

func TestNamespaceLeaseProxyRequiresPrivateNamespace(t *testing.T) {
	f, proxy := proxyFixture(t, RoleShims{Build: true}, "#!/bin/sh\nexit 0\n")
	// BindProcess publishes initialization; set the test exception before any
	// client starts, using a fresh fixture's otherwise idle server.
	proxy.allowHostNamespace.Store(false)
	code, out := f.run(t, f.worktree, nil, filepath.Join(f.shims, "go"), "build", ".")
	if code != HeavyOpRefused || !strings.Contains(out, "private PID namespace") {
		t.Fatalf("host namespace client exit=%d output=%s", code, out)
	}
	if len(f.audit(t)) != 0 {
		t.Fatal("unmapped namespace client queued a lease")
	}
}

func TestNamespaceLeaseProxyRefusesNestedWithoutWaitingForParentLease(t *testing.T) {
	f, _ := proxyFixture(t, RoleShims{Build: true}, "#!/bin/sh\nunset FAE_SHIM_NESTED\ngo build ./...\nexit $?\n")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(f.shims, "go"), "build", "./...")
	cmd.Dir, cmd.Env = f.worktree, f.env
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if ctx.Err() != nil {
		t.Fatal("nested command deadlocked waiting for parent's full-capacity lease", string(out))
	}
	if !errors.As(err, &exit) || exit.ExitCode() != HeavyOpRefused || !strings.Contains(string(out), "FAE-HOST-014") {
		t.Fatalf("nested refusal missing: %v %s", err, out)
	}
	if got := strings.Join(f.audit(t), ","); got != "queued,granted,released" {
		t.Fatal("nested command queued another lease", got)
	}
}

func TestNamespaceLeaseProxyQueuedDisconnectCancelsWaiter(t *testing.T) {
	f, _ := proxyFixture(t, RoleShims{Build: true}, "#!/bin/sh\nsleep 0.5\nexit 0\n")
	first := exec.Command(filepath.Join(f.shims, "go"), "build", "./...")
	first.Dir, first.Env = f.worktree, f.env
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	ledger, err := scheduler.OpenDynamicLedger(f.ledger, 2)
	if err != nil {
		t.Fatal(err)
	}
	waitFor := func(predicate func(scheduler.Pool) bool) {
		t.Helper()
		limit := time.Now().Add(2 * time.Second)
		for {
			pool, e := ledger.Snapshot(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			if predicate(pool) {
				return
			}
			if time.Now().After(limit) {
				t.Fatalf("proxy ledger state not reached: %+v", pool)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor(func(p scheduler.Pool) bool { return len(p.Holders) == 1 })
	second := exec.Command(filepath.Join(f.shims, "go"), "build", "./...")
	second.Dir, second.Env = f.worktree, f.env
	if err = second.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(func(p scheduler.Pool) bool { return len(p.Waiting) == 1 })
	_ = second.Process.Kill()
	_ = second.Wait()
	waitFor(func(p scheduler.Pool) bool { return len(p.Waiting) == 0 && len(p.Holders) == 1 })
	if err = first.Wait(); err != nil {
		t.Fatal(err)
	}
	_ = f.audit(t)
}

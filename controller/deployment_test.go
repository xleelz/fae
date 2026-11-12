package controller

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

func TestOverlapProtection(t *testing.T) {
	for m := 0; m < 32; m++ {
		a, b, c, d, e := m&1 != 0, m&2 != 0, m&4 != 0, m&8 != 0, m&16 != 0
		if MayReleaseOverlap(a, b, c, d, e) != (a || (b && !c && !d && !e)) {
			t.Fatal(m)
		}
	}
}
func deploymentRepository(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		out, e := Git(ctx, root, args...)
		if e != nil {
			t.Fatal(args, e)
		}
		return out
	}
	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.invalid")
	os.WriteFile(filepath.Join(root, "file"), []byte("one"), 0600)
	run("add", ".")
	run("commit", "-m", "one")
	first := run("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(root, "file"), []byte("two"), 0600)
	run("commit", "-am", "two")
	second := run("rev-parse", "HEAD")
	return root, first, second
}
func TestDeploymentFastForward(t *testing.T) {
	root, first, second := deploymentRepository(t)
	ctx := context.Background()
	Git(ctx, root, "update-ref", "refs/heads/integration", first)
	if e := FastForward(ctx, root, "refs/heads/integration", second); e != nil {
		t.Fatal(e)
	}
	if e := FastForward(ctx, root, "refs/heads/integration", second); e != nil {
		t.Fatal(e)
	}
	if e := FastForward(ctx, root, "refs/heads/integration", first); e == nil {
		t.Fatal("rewind accepted")
	}
	got, _ := Git(ctx, root, "rev-parse", "refs/heads/integration")
	if got != second {
		t.Fatal(got)
	}
	if e := FastForward(ctx, root, "HEAD", first); e == nil {
		t.Fatal("nonbranch")
	}
}
func gatedBuildFixture(t *testing.T) (root, tip string) {
	t.Helper()
	root, _, _ = deploymentRepository(t)
	run := func(args ...string) string {
		out, e := Git(context.Background(), root, args...)
		if e != nil {
			t.Fatal(args, e)
		}
		return out
	}
	os.MkdirAll(filepath.Join(root, "cmd", "fae"), 0700)
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.invalid/fixture\n\ngo 1.23\n"), 0600)
	os.WriteFile(filepath.Join(root, "cmd", "fae", "main.go"), []byte("package main\nfunc main() {}\n"), 0600)
	run("add", ".")
	run("commit", "-m", "fixture")
	tip = run("rev-parse", "HEAD")
	run("update-ref", "refs/fae/gated/test", tip)
	return root, tip
}
func buildLeaseFixture(t *testing.T) (BuildLease, *scheduler.Ledger) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host-ledger")
	ledger, e := scheduler.OpenDynamicLedger(path, 2)
	if e != nil {
		t.Fatal(e)
	}
	return BuildLease{Ledger: path, Log: io.Discard, Policy: fastPolicy, Observe: quietHost(64)}, ledger
}

// fastPolicy scales the pressure window to milliseconds on the real clock.
var fastPolicy = hostguard.Policy{Window: 30 * time.Millisecond, MaxGap: time.Second, Interval: 3 * time.Millisecond, Checks: 10, IOGate: 5}

func quietHost(gib float64) func(context.Context) (hostguard.Sample, error) {
	s := hostguard.Sample{Memory: hostguard.Reading{Value: gib * float64(hostguard.GiB), Known: true}, Swap: hostguard.Reading{Value: float64(hostguard.GiB), Known: true}, IO10: hostguard.Reading{Value: 1, Known: true}, Memory10: hostguard.Reading{Value: 0, Known: true}, LoadPerCore: hostguard.Reading{Value: 0.5, Known: true}}
	return func(context.Context) (hostguard.Sample, error) { return s, nil }
}
func TestBuildGatedPinsCleanSource(t *testing.T) {
	root, tip := gatedBuildFixture(t)
	ctx := context.Background()
	lease, _ := buildLeaseFixture(t)
	// Dirty operator checkout must not leak into the build.
	os.WriteFile(filepath.Join(root, "cmd", "fae", "main.go"), []byte("not valid Go"), 0600)
	t.Setenv("GOFLAGS", "-overlay=/nonexistent/poison.json")
	t.Setenv("GOWORK", "/nonexistent/go.work")
	storage := t.TempDir()
	binary, dir, got, e := BuildGated(ctx, lease, root, "HEAD", "refs/fae/gated/test", storage)
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	if filepath.Dir(dir) != storage {
		t.Fatal("deployment not durably placed")
	}
	if _, e := os.Stat(filepath.Join(dir, "source")); !os.IsNotExist(e) {
		t.Fatal("source checkout retained", e)
	}
	if got != tip {
		t.Fatal(got)
	}
	if revision, e := SourceRevision(binary); e != nil || revision != tip {
		t.Fatal(revision, e)
	}
	if e := exec.Command(binary).Run(); e != nil {
		t.Fatal(e)
	}
	if _, _, _, e = BuildGated(ctx, lease, root, "HEAD~1", "refs/fae/gated/test"); e == nil {
		t.Fatal("ungated revision")
	}
	if _, _, _, e = BuildGated(ctx, lease, root, "HEAD", "HEAD"); e == nil {
		t.Fatal("ungated namespace")
	}
	if _, e = SourceRevision(filepath.Join(root, "file")); e == nil {
		t.Fatal("invalid executable")
	}
}

// FAE-SCH-011 clause 7: the controller's package build takes the host-wide
// heavy-op lease (weight 2, controller class), waits FIFO behind holders and
// never runs unleased.
func TestBuildGatedTakesHostHeavyLease(t *testing.T) {
	root, tip := gatedBuildFixture(t)
	ctx := context.Background()
	if _, _, _, e := BuildGated(ctx, BuildLease{}, root, "HEAD", "refs/fae/gated/test", t.TempDir()); e == nil {
		t.Fatal("controller build ran without the host heavy-op ledger")
	}
	lease, ledger := buildLeaseFixture(t)
	owner, e := scheduler.CurrentOwner()
	if e != nil {
		t.Fatal(e)
	}
	blocker := scheduler.Request{ID: "role-compile", Owner: owner, Weight: 2, Queued: time.Now().UTC()}
	if e = ledger.Enqueue(ctx, blocker); e != nil {
		t.Fatal(e)
	}
	if granted, _, e := ledger.Advance(ctx, time.Now().UTC(), map[scheduler.Owner]scheduler.Liveness{owner: scheduler.Alive}); e != nil || len(granted) != 1 {
		t.Fatal(granted, e)
	}
	type result struct {
		binary, dir, tip string
		err              error
	}
	done := make(chan result, 1)
	go func() {
		b, d, got, e := BuildGated(ctx, lease, root, "HEAD", "refs/fae/gated/test", t.TempDir())
		done <- result{b, d, got, e}
	}()
	var waiter scheduler.Request
	for deadline := time.Now().Add(10 * time.Second); waiter.ID == ""; {
		pool, e := ledger.Snapshot(ctx)
		if e != nil {
			t.Fatal(e)
		}
		for _, w := range pool.Waiting {
			waiter = w
		}
		if time.Now().After(deadline) {
			t.Fatal("controller build never queued for the heavy-op lease")
		}
		time.Sleep(time.Millisecond)
	}
	if waiter.Weight != 2 || waiter.Class != 1 || waiter.Owner != owner {
		t.Fatalf("controller build lease %+v", waiter)
	}
	select {
	case r := <-done:
		t.Fatal("controller build ran while the pool was full", r.err)
	case <-time.After(100 * time.Millisecond):
	}
	if e = ledger.Release(ctx, blocker.ID, owner, "role compile finished"); e != nil {
		t.Fatal(e)
	}
	r := <-done
	if r.err != nil || r.tip != tip {
		t.Fatal(r.err)
	}
	defer os.RemoveAll(r.dir)
	pool, e := ledger.Snapshot(ctx)
	if e != nil || len(pool.Holders) != 0 || len(pool.Waiting) != 0 {
		t.Fatal("controller build lease leaked", pool, e)
	}
	audit, e := ledger.Audit(ctx)
	if e != nil {
		t.Fatal(e)
	}
	order := []string{}
	for _, a := range audit {
		if a.ID == waiter.ID || a.ID == blocker.ID {
			order = append(order, a.ID+":"+a.Action)
		}
	}
	if strings.Join(order, ",") != "role-compile:queued,role-compile:granted,"+waiter.ID+":queued,role-compile:released,"+waiter.ID+":granted,"+waiter.ID+":released" {
		t.Fatal("controller build lease order", order)
	}
}

// FAE-HOST-001, -007, -010, -020, -031: the controller roll's package build
// samples the host before it queues, waits (bounded) for the landing floor and
// the two-reading I/O gate, refuses at once on the hard breaker, reclaims a
// dead owner's entry, clamps build jobs and keeps its scratch on disk.
func TestBuildGatedPassesHostAdmission(t *testing.T) {
	root, tip := gatedBuildFixture(t)
	ctx := context.Background()
	real, e := exec.LookPath("go")
	if e != nil {
		t.Skip("go toolchain unavailable")
	}
	tools := t.TempDir()
	record := filepath.Join(t.TempDir(), "go-invocation")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + record + "\nenv >> " + record + "\nexec " + real + " \"$@\"\n"
	if e = os.WriteFile(filepath.Join(tools, "go"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", t.TempDir())

	lease, ledger := buildLeaseFixture(t)
	lease.Observe, lease.MaxWait = quietHost(11.5), 100*time.Millisecond
	var timeout *AdmissionTimeout
	if _, _, _, e = BuildGated(ctx, lease, root, "HEAD", "refs/fae/gated/test", t.TempDir()); !errors.As(e, &timeout) || timeout.Stage != "memory" {
		t.Fatal("build below the landing floor was not held to infrastructure", e)
	}
	lease.Observe = quietHost(1)
	var refused *AdmissionRefused
	if _, _, _, e = BuildGated(ctx, lease, root, "HEAD", "refs/fae/gated/test", t.TempDir()); !errors.As(e, &refused) || refused.Reason != "HostHardLimit" {
		t.Fatal("hard breaker did not refuse the build", e)
	}
	if _, e = os.Stat(record); !os.IsNotExist(e) {
		t.Fatal("build ran without host admission")
	}
	pool, e := ledger.Snapshot(ctx)
	if e != nil || len(pool.Holders)+len(pool.Waiting) != 0 {
		t.Fatal("refused build left a lease entry", pool, e)
	}

	dead := scheduler.Owner{PID: 4242, Started: 77}
	if e = ledger.Enqueue(ctx, scheduler.Request{ID: "crashed-build", Owner: dead, Weight: 2, Queued: time.Now().UTC()}); e != nil {
		t.Fatal(e)
	}
	if _, _, e = ledger.Advance(ctx, time.Now().UTC(), map[scheduler.Owner]scheduler.Liveness{}); e != nil {
		t.Fatal(e)
	}
	readings, gapped := 0, false
	var first, last time.Time
	lease.MaxWait = time.Minute
	lease.Probe = func(o scheduler.Owner) scheduler.Liveness {
		if o == dead {
			return scheduler.Dead
		}
		return scheduler.Unknown
	}
	quiet := quietHost(64)
	lease.Observe = func(ctx context.Context) (hostguard.Sample, error) {
		readings++
		if first.IsZero() {
			first = time.Now()
		}
		last = time.Now()
		if p, _ := ledger.Snapshot(ctx); len(p.Waiting) > 0 && readings < 2 {
			gapped = true
		}
		return quiet(ctx)
	}
	binary, dir, got, e := BuildGated(ctx, lease, root, "HEAD", "refs/fae/gated/test", t.TempDir())
	if e != nil || got != tip {
		t.Fatal("dead owner's entry was not reclaimed", e)
	}
	defer os.RemoveAll(dir)
	if gapped || readings < 3 || last.Sub(first) < fastPolicy.Window {
		t.Fatal("build queued before two I/O readings a window apart and a reading under the lease", readings, last.Sub(first))
	}
	raw, e := os.ReadFile(record)
	if e != nil {
		t.Fatal(e)
	}
	lines := strings.Split(string(raw), "\n")
	scratch := filepath.Join(dir, "tmp")
	if !strings.HasPrefix(lines[0], "build -p 2 ") || !strings.Contains(string(raw), "\nTMPDIR="+scratch+"\n") || !strings.Contains(string(raw), "\nGOTMPDIR="+scratch+"\n") {
		t.Fatal("build not clamped or its scratch not on disk", lines[0])
	}
	if _, e = os.Stat(scratch); !os.IsNotExist(e) {
		t.Fatal("build scratch not removed", e)
	}
	if _, e = os.Stat(binary); e != nil {
		t.Fatal(e)
	}
	audit, e := ledger.Audit(ctx)
	if e != nil {
		t.Fatal(e)
	}
	reclaimed := false
	for _, entry := range audit {
		for _, ev := range entry.Events {
			reclaimed = reclaimed || ev.Kind == "Reclaimed" && ev.RequestID == "crashed-build"
		}
	}
	if !reclaimed {
		t.Fatal("dead owner's reclamation not audited")
	}
}

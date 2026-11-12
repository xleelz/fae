package commandshim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

func healthyHost(context.Context) (hostguard.Sample, error) {
	r := func(v float64) hostguard.Reading { return hostguard.Reading{Value: v, Known: true} }
	return hostguard.Sample{Memory: r(64 * float64(hostguard.GiB)), Swap: r(1), IO10: r(0), IO60: r(0), Memory10: r(0), Memory60: r(0), LoadPerCore: r(0.1)}, nil
}

// fastPolicy scales the pressure window to milliseconds on the real clock.
var fastPolicy = hostguard.Policy{Window: 30 * time.Millisecond, MaxGap: time.Second, Interval: 3 * time.Millisecond, Checks: 10, IOGate: 5}

func heavyLedger(t *testing.T, capacity int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host")
	if _, e := scheduler.OpenLedger(path, capacity); e != nil {
		t.Fatal(e)
	}
	return path
}

func TestStripLeaseMarkers(t *testing.T) {
	got := StripLeaseMarkers([]string{"PATH=/bin", LeaseMarker + "=parent", "FAE_HEAVY_OP_CLASS=role", "FAE_HOST_LEDGER=/l", "X=1"})
	if strings.Join(got, ",") != "PATH=/bin,FAE_HOST_LEDGER=/l,X=1" {
		t.Fatal(got)
	}
}

func TestHeavyOpRunsLeasedStripsMarkerAndPassesExitCode(t *testing.T) {
	ledger := heavyLedger(t, 2)
	out := filepath.Join(t.TempDir(), "env")
	var stderr bytes.Buffer
	code, err := RunHeavyOp(context.Background(), HeavyOp{Ledger: ledger, Weight: 1, Class: "role", Argv: []string{"/bin/sh", "-c", "env > " + out + "; exit 3"}, Env: []string{"PATH=/usr/bin:/bin", LeaseMarker + "=parent-permit", "KEEP=1"}, Observe: healthyHost, Policy: fastPolicy, Stderr: &stderr})
	if err != nil || code != 3 {
		t.Fatal(code, err, stderr.String())
	}
	env, e := os.ReadFile(out)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(env), "FAE_HEAVY_OP_") || !strings.Contains(string(env), "KEEP=1") {
		t.Fatalf("child environment %s", env)
	}
	l, _ := scheduler.OpenDynamicLedger(ledger, 2)
	pool, _ := l.Snapshot(context.Background())
	audit, _ := l.Audit(context.Background())
	actions := []string{}
	for _, a := range audit {
		actions = append(actions, a.Action)
	}
	if len(pool.Holders) != 0 || len(pool.Waiting) != 0 || strings.Join(actions, ",") != "queued,granted,released" {
		t.Fatal(pool, actions)
	}
	if code, err = RunHeavyOp(context.Background(), HeavyOp{Ledger: ledger, Weight: 2, Class: "operator", Argv: []string{"/bin/true"}, Observe: healthyHost, Policy: fastPolicy}); err != nil || code != 0 {
		t.Fatal(code, err)
	}
}

func TestHeavyOpRefusalsAreDistinctAndLeaveNoLease(t *testing.T) {
	ledger := heavyLedger(t, 2)
	hard := func(context.Context) (hostguard.Sample, error) {
		s, _ := healthyHost(context.Background())
		s.Memory.Value = float64(hostguard.GiB) / 2
		return s, nil
	}
	ran := filepath.Join(t.TempDir(), "ran")
	for name, h := range map[string]HeavyOp{
		"missing ledger":  {Ledger: filepath.Join(t.TempDir(), "absent"), Weight: 1, Class: "role", Argv: []string{"/bin/true"}},
		"relative ledger": {Ledger: "host", Weight: 1, Class: "role", Argv: []string{"/bin/true"}},
		"zero weight":     {Ledger: ledger, Weight: 0, Class: "role", Argv: []string{"/bin/true"}},
		"over capacity":   {Ledger: ledger, Weight: 3, Class: "role", Argv: []string{"/bin/true"}},
		"unknown class":   {Ledger: ledger, Weight: 1, Class: "root", Argv: []string{"/bin/true"}},
		"empty command":   {Ledger: ledger, Weight: 1, Class: "role"},
		"host guard":      {Ledger: ledger, Weight: 1, Class: "role", Argv: []string{"/bin/sh", "-c", "touch " + ran}, Observe: hard, Policy: fastPolicy},
	} {
		code, err := RunHeavyOp(context.Background(), h)
		var refusal *HeavyOpRefusal
		if !errors.As(err, &refusal) || code != HeavyOpRefused || HeavyOpRefused == Refused {
			t.Fatal(name, code, err)
		}
	}
	if _, e := os.Stat(ran); !os.IsNotExist(e) {
		t.Fatal("refused command ran")
	}
	// A full pool is waited on, then refused at the bound; the request is withdrawn.
	l, _ := scheduler.OpenDynamicLedger(ledger, 2)
	owner, _ := scheduler.CurrentOwner()
	ctx := context.Background()
	if e := l.Enqueue(ctx, scheduler.Request{ID: "holder", Owner: owner, Weight: 2, Queued: time.Now().UTC()}); e != nil {
		t.Fatal(e)
	}
	if _, _, e := l.Advance(ctx, time.Now().UTC(), map[scheduler.Owner]scheduler.Liveness{owner: scheduler.Alive}); e != nil {
		t.Fatal(e)
	}
	code, err := RunHeavyOp(ctx, HeavyOp{Ledger: ledger, Weight: 1, Class: "controller", Argv: []string{"/bin/sh", "-c", "touch " + ran}, Observe: healthyHost, Policy: fastPolicy, MaxWait: 300 * time.Millisecond})
	var refusal *HeavyOpRefusal
	if !errors.As(err, &refusal) || code != HeavyOpRefused {
		t.Fatal(code, err)
	}
	if pool, _ := l.Snapshot(ctx); len(pool.Waiting) != 0 || len(pool.Holders) != 1 {
		t.Fatal("waiter left behind", pool)
	}
	if _, e := os.Stat(ran); !os.IsNotExist(e) {
		t.Fatal("unleased command ran")
	}
}

// FAE-HOST-002, -003, -005, -010, -031 on the broker path: heavy-op run shares
// the daemon-side admission. The I/O gate needs two readings a window apart
// before the lease is requested; soft pressure backs off for at most the
// policy's checks and refuses without a lease; a hard refusal is spooled for
// the owning daemon's alert; a proven-dead owner's entry is reclaimed.
func TestHeavyOpSharesHostAdmission(t *testing.T) {
	ctx := context.Background()
	ledger := heavyLedger(t, 2)
	l, _ := scheduler.OpenDynamicLedger(ledger, 2)
	ran := filepath.Join(t.TempDir(), "ran")
	var first, ranAt time.Time
	queuedEarly := false
	readings := 0
	gate := func(ctx context.Context) (hostguard.Sample, error) {
		readings++
		if first.IsZero() {
			first = time.Now()
		}
		if p, _ := l.Snapshot(ctx); len(p.Waiting)+len(p.Holders) > 0 && time.Since(first) < fastPolicy.Window {
			queuedEarly = true
		}
		return healthyHost(ctx)
	}
	code, err := RunHeavyOp(ctx, HeavyOp{Ledger: ledger, Weight: 1, Class: "role", Argv: []string{"/bin/sh", "-c", "touch " + ran}, Observe: gate, Policy: fastPolicy})
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	if info, e := os.Stat(ran); e == nil {
		ranAt = info.ModTime()
	}
	if queuedEarly || readings < 3 || ranAt.Sub(first) < fastPolicy.Window-time.Millisecond {
		t.Fatal("lease requested before the two-reading I/O gate passed", queuedEarly, readings, ranAt.Sub(first))
	}
	os.Remove(ran)

	loaded := func(ctx context.Context) (hostguard.Sample, error) {
		readings++
		s, _ := healthyHost(ctx)
		s.LoadPerCore.Value = 4.5
		return s, nil
	}
	readings = 0
	var stderr bytes.Buffer
	code, err = RunHeavyOp(ctx, HeavyOp{Ledger: ledger, Weight: 1, Class: "role", Argv: []string{"/bin/sh", "-c", "touch " + ran}, Observe: loaded, Policy: fastPolicy, Stderr: &stderr})
	var refusal *HeavyOpRefusal
	if !errors.As(err, &refusal) || code != HeavyOpRefused || readings != fastPolicy.Checks+1 || !strings.Contains(err.Error(), "pressure") {
		t.Fatal("soft pressure was not backed off and refused", code, err, readings)
	}
	if _, e := os.Stat(ran); !os.IsNotExist(e) {
		t.Fatal("command ran under pressure")
	}

	spool := filepath.Join(t.TempDir(), "host-alerts")
	if e := os.Mkdir(spool, 0700); e != nil {
		t.Fatal(e)
	}
	hard := func(ctx context.Context) (hostguard.Sample, error) {
		s, _ := healthyHost(ctx)
		s.Memory.Value = float64(hostguard.GiB) / 2
		return s, nil
	}
	code, err = RunHeavyOp(ctx, HeavyOp{Ledger: ledger, Weight: 1, Class: "role", Argv: []string{"/bin/true"}, Observe: hard, Policy: fastPolicy, AlertSpool: spool, Stderr: &stderr})
	if !errors.As(err, &refusal) || code != HeavyOpRefused || !strings.Contains(err.Error(), "HostHardLimit") {
		t.Fatal(code, err)
	}
	entries, _ := os.ReadDir(spool)
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatal("hard refusal not spooled", entries)
	}
	raw, _ := os.ReadFile(filepath.Join(spool, entries[0].Name()))
	var alert HostAlert
	if json.Unmarshal(raw, &alert) != nil || alert.Reason != "HostHardLimit" || alert.MemoryAvailable == nil || *alert.MemoryAvailable != float64(hostguard.GiB)/2 {
		t.Fatal(string(raw))
	}
	if pool, _ := l.Snapshot(ctx); len(pool.Holders)+len(pool.Waiting) != 0 {
		t.Fatal("refusals left lease entries", pool)
	}

	dead := scheduler.Owner{PID: 4242, Started: 77}
	if e := l.Enqueue(ctx, scheduler.Request{ID: "crashed", Owner: dead, Weight: 2, Queued: time.Now().UTC()}); e != nil {
		t.Fatal(e)
	}
	if _, _, e := l.Advance(ctx, time.Now().UTC(), map[scheduler.Owner]scheduler.Liveness{}); e != nil {
		t.Fatal(e)
	}
	probe := func(o scheduler.Owner) scheduler.Liveness {
		if o == dead {
			return scheduler.Dead
		}
		return scheduler.Unknown
	}
	if code, err = RunHeavyOp(ctx, HeavyOp{Ledger: ledger, Weight: 1, Class: "role", Argv: []string{"/bin/true"}, Observe: healthyHost, Policy: fastPolicy, Probe: probe, MaxWait: 2 * time.Second}); err != nil || code != 0 {
		t.Fatal("dead owner's entry was not reclaimed", code, err)
	}
}

// heavyOpClock is a deterministic clock for the broker's admission waits.
type heavyOpClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *heavyOpClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *heavyOpClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

// FAE-HOST-005 on the role-shim lease path: heavy-op run's slice is its
// whole maximum wait, so before, a soft-pressure wait recorded no reading
// unless it timed out, and then only the last. Now the reading that starts
// the hold is recorded at once, in the controller's reading form, and so is
// the reading that lifts it. Under the default policy a role command that
// meets pressure refuses at its tenth re-check (the hold needs two clear
// readings 30 s apart after the first clear one), so the record is the first
// held reading and the timeout's; with more re-checks configured the hold
// lifts within the wait and its lifting reading is recorded.
func TestHeavyOpRecordsTheReadingsItWaitsOn(t *testing.T) {
	loaded, _ := healthyHost(context.Background())
	loaded.LoadPerCore.Value, loaded.IO10.Value, loaded.IO60.Value = 4.5, 1.5, 0.75
	run := func(t *testing.T, policy hostguard.Policy) (int, error, []string) {
		t.Helper()
		reads := 0
		observe := func(ctx context.Context) (hostguard.Sample, error) {
			reads++
			if reads == 1 {
				return loaded, nil
			}
			return healthyHost(ctx)
		}
		var stderr bytes.Buffer
		clock := &heavyOpClock{now: time.Date(2026, 10, 28, 1, 2, 3, 0, time.UTC)}
		code, err := RunHeavyOp(context.Background(), HeavyOp{Ledger: heavyLedger(t, 2), Weight: 1, Class: "role", Argv: []string{"/bin/true"}, Observe: observe, Clock: clock, Policy: policy, Stderr: &stderr})
		return code, err, strings.Split(strings.TrimSpace(stderr.String()), "\n")
	}
	line := func(t *testing.T, lines []string, prefix string, fields ...string) {
		t.Helper()
		for _, l := range lines {
			// Each line is <time> instance=<name> component=heavy-op <text>.
			if _, text, ok := strings.Cut(l, " component=heavy-op "); !ok || !strings.HasPrefix(text, prefix) {
				continue
			}
			for _, f := range fields {
				if !slices.Contains(strings.Fields(l), f) {
					t.Fatalf("%q lacks %s", l, f)
				}
			}
			return
		}
		t.Fatalf("no line starts with %q: %q", prefix, lines)
	}
	first := []string{"load_per_core=4.50", "io_full_avg10=1.50", "io_full_avg60=0.75", "io_avg10_dir=unknown", "memory_full_avg10=0.00", "memory_avg60_dir=unknown"}

	code, err, lines := run(t, hostguard.DefaultPolicy())
	if code != HeavyOpRefused || err == nil {
		t.Fatal("soft pressure on the first reading was not refused at the bound", code, err)
	}
	line(t, lines, "gate waiting stage=pressure id=heavy-op-", first...)
	line(t, lines, "gate timeout stage=pressure id=heavy-op-", "load_per_core=0.10", "io_avg10_dir=steady")
	if len(lines) != 2 {
		t.Fatalf("want the first held reading and the timeout, got %q", lines)
	}

	policy := hostguard.DefaultPolicy()
	policy.Checks = 20
	code, err, lines = run(t, policy)
	if code != 0 || err != nil {
		t.Fatal("the hold did not lift within the configured re-checks", code, err, lines)
	}
	line(t, lines, "gate waiting stage=pressure id=heavy-op-", first...)
	line(t, lines, "gate lifted stage=pressure id=heavy-op-", "load_per_core=0.10", "io_full_avg10=0.00", "io_avg10_dir=steady")
	if len(lines) != 2 {
		t.Fatalf("want the first held reading and its lift, got %q", lines)
	}
}

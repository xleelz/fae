package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
)

// productionPressurePolicy captures the production selector before init
// replaces it for this test binary.
var productionPressurePolicy = livePressurePolicy

// Daemon integration tests drive live workflows against real host telemetry
// with 60-second state bounds. Only this test binary scales the pressure window
// and back-off interval; every pressure test below injects
// hostguard.DefaultPolicy() and a deterministic clock to exercise the
// production 30 s window and 10 x 3 s back-off.
//
// Live tests admit against the real host's PSI and load by default, so the
// suite exercises host pressure as production does. For development runs on a
// shared, loaded host only, FAE_TEST_QUIET_HOST_PRESSURE=1 reports PSI and load
// as quiet; available memory and swap stay real, so the hard floor still
// protects the host.
func init() {
	livePressurePolicy = func() hostguard.Policy {
		p := hostguard.DefaultPolicy()
		p.Window, p.Interval = 30*time.Millisecond, 3*time.Millisecond
		return p
	}
	if os.Getenv("FAE_TEST_QUIET_HOST_PRESSURE") == "1" {
		liveObserve = quietPressure(hostguard.Observe)
	}
}

func quietPressure(observe func(context.Context) (hostguard.Sample, error)) func(context.Context) (hostguard.Sample, error) {
	return func(ctx context.Context) (hostguard.Sample, error) {
		s, e := observe(ctx)
		quiet := hostguard.Reading{Value: 0, Known: true}
		s.IO10, s.IO60, s.Memory10, s.Memory60, s.LoadPerCore = quiet, quiet, quiet, quiet, quiet
		return s, e
	}
}

type pressureClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func (c *pressureClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *pressureClock) Sleep(ctx context.Context, d time.Duration) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	c.mu.Unlock()
	return ctx.Err()
}
func (c *pressureClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
func (c *pressureClock) elapsed(since time.Time) time.Duration {
	return c.Now().Sub(since)
}

var pressureEpoch = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// pressureFixture is admissionFixture with the production pressure policy, a
// deterministic clock and a daemon log file.
func pressureFixture(t *testing.T, capacity int) (*liveAdmission, *pressureClock, string) {
	t.Helper()
	a := admissionFixture(t, capacity)
	clock := &pressureClock{now: pressureEpoch}
	policy := hostguard.DefaultPolicy()
	a.clock, a.policy = clock, &policy
	path := filepath.Join(t.TempDir(), "daemon.log")
	f, e := os.Create(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.Close() })
	a.daemon.log = f
	return a, clock, path
}

func hostSample(io, load float64) hostguard.Sample {
	return hostguard.Sample{
		Memory: hostguard.Reading{Value: float64(32 * hostguard.GiB), Known: true}, Swap: hostguard.Reading{Value: float64(hostguard.GiB), Known: true},
		IO10: hostguard.Reading{Value: io, Known: true}, IO60: hostguard.Reading{Value: io, Known: true},
		Memory10: hostguard.Reading{Known: true}, Memory60: hostguard.Reading{Known: true},
		LoadPerCore: hostguard.Reading{Value: load, Known: true},
	}
}

type poolView struct{ holders, waiting int }

func poolCounts(t *testing.T, a *liveAdmission) poolView {
	t.Helper()
	p, e := a.host.Snapshot(context.Background())
	if e != nil {
		t.Error(e)
	}
	return poolView{len(p.Holders), len(p.Waiting)}
}

func refusalReason(t *testing.T, e error) string {
	t.Helper()
	var deferral interface{ AdmissionDeferred() bool }
	var refusal *scheduler.AdmissionRefusal
	if e == nil || !errors.As(e, &deferral) || !deferral.AdmissionDeferred() || !errors.As(e, &refusal) {
		t.Fatalf("expected a deferred admission refusal, got %v", e)
	}
	return refusal.Reason
}

// hostAdmissionLines returns the structured host admission log lines and
// checks that each is key=value and names its gate, outcome and reason.
func hostAdmissionLines(t *testing.T, path string) []string {
	t.Helper()
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if !strings.HasPrefix(line, "host admission ") {
			continue
		}
		fields := strings.Fields(line)
		seen := map[string]bool{}
		for _, f := range fields[3:] {
			k, v, ok := strings.Cut(f, "=")
			if !ok || k == "" || v == "" {
				t.Fatalf("unstructured field %q in %q", f, line)
			}
			seen[k] = true
		}
		for _, k := range []string{"gate", "outcome", "reason", "id", "session"} {
			if !seen[k] {
				t.Fatalf("missing %s= in %q", k, line)
			}
		}
		out = append(out, line)
	}
	return out
}

func countContaining(lines []string, parts ...string) int {
	n := 0
	for _, line := range lines {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			n++
		}
	}
	return n
}

func heavyDispatch(p workflow.Primitive) engine.Dispatch {
	return engine.Dispatch{TaskID: "heavy", Config: workflow.Stage{Primitive: p, Weight: 1}}
}

// FAE-HOST-010: the heavy-op lease is not requested until two I/O readings at
// least 30 s apart are below the gate threshold.
func TestLiveAdmissionIOGatePassesBeforeHeavyLeaseEnqueue(t *testing.T) {
	for _, primitive := range []workflow.Primitive{workflow.HostCommand, workflow.VcsCommit, workflow.Integrate} {
		t.Run(string(primitive), func(t *testing.T) {
			a, clock, logPath := pressureFixture(t, 1)
			var seen []poolView
			a.observe = func(context.Context) (hostguard.Sample, error) {
				seen = append(seen, poolCounts(t, a))
				return hostSample(1, 0.5), nil
			}
			release, e := a.Acquire(context.Background(), heavyDispatch(primitive), execution.Binding{}, engine.Session{ID: "gate-" + string(primitive)})
			if e != nil {
				t.Fatal(e)
			}
			// 11 gate readings 3 s apart (0..30 s), all before any queue entry,
			// then the pre-launch reading under the granted lease.
			if len(seen) != 12 {
				t.Fatalf("readings %d: %v", len(seen), seen)
			}
			for i, v := range seen[:11] {
				if v != (poolView{0, 0}) {
					t.Fatalf("gate reading %d saw heavy lease state %+v", i, v)
				}
			}
			if seen[11] != (poolView{1, 0}) {
				t.Fatalf("pre-launch reading must hold the lease: %+v", seen[11])
			}
			if clock.elapsed(pressureEpoch) != 30*time.Second || len(clock.slept) != 10 {
				t.Fatal("gate window", clock.elapsed(pressureEpoch), clock.slept)
			}
			if e = release(); e != nil {
				t.Fatal(e)
			}
			assertHostCounts(t, a, 0, 0)
			lines := hostAdmissionLines(t, logPath)
			if countContaining(lines, "gate=io", "outcome=waiting", "reason=IOPressureHold", "io_full_avg10=1.00", "io_full_avg60=1.00", "io_avg10_dir=") != 10 {
				t.Fatalf("waiting readings not recorded:\n%s", strings.Join(lines, "\n"))
			}
			if countContaining(lines, "gate=io", "outcome=proceed", "reason=IOGateClear", "lifted=true") != 1 {
				t.Fatalf("gate pass not recorded:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

// Sustained I/O pressure refuses (bounded) without ever enqueueing the lease;
// a gate still waiting for its second reading is a distinct refusal whose
// chain survives the engine's park/retry.
func TestLiveAdmissionIOGatePressureAndWaitingRefusalsTakeNoLease(t *testing.T) {
	a, clock, logPath := pressureFixture(t, 1)
	readings := 0
	a.observe = func(context.Context) (hostguard.Sample, error) {
		readings++
		if v := poolCounts(t, a); v != (poolView{0, 0}) {
			t.Errorf("gate reading saw lease state %+v", v)
		}
		return hostSample(50, 0.5), nil
	}
	release, e := a.Acquire(context.Background(), heavyDispatch(workflow.HostCommand), execution.Binding{}, engine.Session{ID: "io-pressure"})
	if release != nil || refusalReason(t, e) != "IOPressure" || readings != 11 || len(clock.slept) != 10 {
		t.Fatal("sustained pressure", release != nil, e, readings, clock.slept)
	}
	assertHostCounts(t, a, 0, 0)

	// Exactly at the gate threshold is not "below" (FAE-HOST-010).
	readings = 0
	a.observe = func(context.Context) (hostguard.Sample, error) {
		readings++
		return hostSample(hostguard.DefaultPolicy().IOGate, 0.5), nil
	}
	if _, e = a.Acquire(context.Background(), heavyDispatch(workflow.HostCommand), execution.Binding{}, engine.Session{ID: "io-at-threshold"}); refusalReason(t, e) != "IOPressure" || readings != 11 {
		t.Fatal("at threshold", e, readings)
	}
	assertHostCounts(t, a, 0, 0)

	// A shorter bound ends while the clear chain is still waiting.
	short := hostguard.DefaultPolicy()
	short.Checks = 4
	b, clock, _ := pressureFixture(t, 1)
	b.policy = &short
	start := clock.Now()
	b.observe = func(context.Context) (hostguard.Sample, error) { return hostSample(1, 0.5), nil }
	if _, e = b.Acquire(context.Background(), heavyDispatch(workflow.HostCommand), execution.Binding{}, engine.Session{ID: "io-waiting"}); refusalReason(t, e) != "IOPressureHold" {
		t.Fatal("waiting", e)
	}
	assertHostCounts(t, b, 0, 0)
	// The engine retries a parked deferral later; the chain (0..12 s) is
	// continued by the next reading at 32 s, which lifts the gate at once.
	clock.advance(20 * time.Second)
	release, e = b.Acquire(context.Background(), heavyDispatch(workflow.HostCommand), execution.Binding{}, engine.Session{ID: "io-retry"})
	if e != nil || clock.elapsed(start) != 32*time.Second {
		t.Fatal("retry", e, clock.elapsed(start))
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
	lines := hostAdmissionLines(t, logPath)
	if countContaining(lines, "host admission refused", "gate=io", "outcome=refused", "reason=IOPressure ", "checks=11") != 2 {
		t.Fatalf("refusals not recorded:\n%s", strings.Join(lines, "\n"))
	}
}

// Unreadable I/O pressure proceeds and records the gap (FAE-HOST-004).
func TestLiveAdmissionIOGateUnknownTelemetryProceedsWithGap(t *testing.T) {
	a, clock, logPath := pressureFixture(t, 1)
	readings := 0
	a.observe = func(context.Context) (hostguard.Sample, error) {
		readings++
		s := hostSample(0, 0.5)
		s.IO10, s.IO60, s.Memory10, s.Memory60 = hostguard.Reading{}, hostguard.Reading{}, hostguard.Reading{}, hostguard.Reading{}
		return s, nil
	}
	release, e := a.Acquire(context.Background(), heavyDispatch(workflow.HostCommand), execution.Binding{}, engine.Session{ID: "io-gap"})
	if e != nil || readings != 2 || len(clock.slept) != 0 {
		t.Fatal(e, readings, clock.slept)
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
	// The I/O gate reads once before the lease; the pressure hold reads then
	// and again under the granted lease. Every gap is recorded.
	lines := hostAdmissionLines(t, logPath)
	for gate, want := range map[string]int{"gate=io": 1, "gate=pressure": 2} {
		if countContaining(lines, gate, "outcome=proceed", "reason=TelemetryGap", "gap=true", "missing=io_full_avg10,io_full_avg60,memory_full_avg10,memory_full_avg60") != want {
			t.Fatalf("%s gap not recorded:\n%s", gate, strings.Join(lines, "\n"))
		}
	}
}

// Cancellation during a real 3 s back-off sleep returns promptly and leaves
// no lease or queue entry.
func TestLiveAdmissionIOGateCancellationReturnsPromptly(t *testing.T) {
	a, _, _ := pressureFixture(t, 1)
	a.clock = nil // the real clock and the production 3 s interval
	readings := 0
	a.observe = func(context.Context) (hostguard.Sample, error) { readings++; return hostSample(50, 0.5), nil }
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	started := time.Now()
	release, e := a.Acquire(ctx, heavyDispatch(workflow.HostCommand), execution.Binding{}, engine.Session{ID: "io-cancel"})
	if release != nil || !errors.Is(e, context.Canceled) || time.Since(started) > 2*time.Second || readings != 1 {
		t.Fatal(release != nil, e, time.Since(started), readings)
	}
	assertHostCounts(t, a, 0, 0)
}

// The hard breaker refuses at once, without back-off, on both admission paths.
func TestLiveAdmissionHardBreakerStopsWithoutBackoff(t *testing.T) {
	for _, primitive := range []workflow.Primitive{workflow.HostCommand, ""} {
		a, clock, logPath := pressureFixture(t, 1)
		readings := 0
		a.observe = func(context.Context) (hostguard.Sample, error) {
			readings++
			s := hostSample(1, 0.5)
			s.Memory.Value = float64(hostguard.GiB)
			return s, nil
		}
		release, e := a.Acquire(context.Background(), heavyDispatch(primitive), execution.Binding{}, engine.Session{ID: "hard-" + string(primitive)})
		if release != nil || refusalReason(t, e) != "HostHardLimit" || readings != 1 || len(clock.slept) != 0 {
			t.Fatal(primitive, e, readings, clock.slept)
		}
		assertHostCounts(t, a, 0, 0)
		if countContaining(hostAdmissionLines(t, logPath), "host admission refused", "outcome=stop", "reason=HostHardLimit", "checks=1") != 1 {
			t.Fatal("hard stop not recorded")
		}
	}
}

// FAE-HOST-002/-005 through live admission: soft pressure backs off (bounded)
// and refuses; the resulting hold is lifted only by two clear readings at
// least 30 s apart, and a reading above threshold in between keeps it. The
// back-off runs before the heavy-op lease is requested, so the lease is never
// held while backing off; one fresh reading is taken under the granted lease.
func TestLiveAdmissionPressureHoldLiftsOnlyOnTwoReadings(t *testing.T) {
	a, clock, logPath := pressureFixture(t, 1)
	c := hostguard.DefaultConfig()
	var script func(n int) hostguard.Sample
	readings, leased := 0, 0
	a.observe = func(context.Context) (hostguard.Sample, error) {
		switch v := poolCounts(t, a); v {
		case poolView{1, 0}:
			leased++
		case poolView{0, 0}:
			if leased > 0 {
				t.Error("back-off reading after the lease reading")
			}
		default:
			t.Errorf("reading with lease state %+v", v)
		}
		readings++
		return script(readings), nil
	}
	session := func(id string) engine.Session { return engine.Session{ID: id} }
	// Just above the load threshold: back off 10 x 3 s, then refuse and park.
	script = func(int) hostguard.Sample { return hostSample(1, c.Load+0.01) }
	if _, e := a.Acquire(context.Background(), engine.Dispatch{TaskID: "soft"}, execution.Binding{}, session("soft-1")); refusalReason(t, e) != "HostPressure" || readings != 11 || leased != 0 {
		t.Fatal("pressure", e, readings, leased)
	}
	assertHostCounts(t, a, 0, 0)
	hold := a.pressureState.hold
	if !hold.Active() {
		t.Fatal("pressure did not enter hold")
	}
	// Clear, then above threshold, then clear: the chain restarts at 6 s and
	// is only 24 s old at the bound, so the hold stays (waiting refusal).
	readings = 0
	script = func(n int) hostguard.Sample {
		if n == 2 {
			return hostSample(c.IO+0.01, 0.5)
		}
		return hostSample(1, 0.5)
	}
	if _, e := a.Acquire(context.Background(), engine.Dispatch{TaskID: "soft"}, execution.Binding{}, session("soft-2")); refusalReason(t, e) != "HostPressureHold" || readings != 11 || !hold.Active() {
		t.Fatal("second reading above threshold must keep the hold", e, readings)
	}
	assertHostCounts(t, a, 0, 0)
	// Engine retry 5 s later: 29 s after the chain start still waits; the next
	// reading 3 s later lifts the hold, then the lease is granted and read once.
	clock.advance(5 * time.Second)
	readings = 0
	script = func(int) hostguard.Sample { return hostSample(1, 0.5) }
	release, e := a.Acquire(context.Background(), engine.Dispatch{TaskID: "soft"}, execution.Binding{}, session("soft-3"))
	if e != nil || readings != 3 || leased != 1 || hold.Active() {
		t.Fatal("lift", e, readings, leased)
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
	// After the hold, a value exactly at a soft threshold proceeds on its single
	// back-off reading and its reading under the lease (FAE-HOST-002).
	readings, leased = 0, 0
	script = func(int) hostguard.Sample { return hostSample(c.IO, c.Load) }
	release, e = a.Acquire(context.Background(), engine.Dispatch{TaskID: "soft"}, execution.Binding{}, session("soft-4"))
	if e != nil || readings != 2 || leased != 1 {
		t.Fatal("exact threshold", e, readings, leased)
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
	lines := hostAdmissionLines(t, logPath)
	for _, want := range [][]string{
		{"host admission refused", "gate=pressure", "outcome=refused", "reason=HostPressure ", "checks=11"},
		{"host admission refused", "gate=pressure", "outcome=waiting", "reason=HostPressureHold", "checks=11"},
		{"host admission reading", "gate=pressure", "outcome=refused", "reason=HostPressure ", "held=true", "io_full_avg10=10.01", "io_avg10_dir=rising"},
		{"host admission reading", "gate=pressure", "outcome=proceed", "reason=PressureHoldLifted", "lifted=true"},
	} {
		if countContaining(lines, want...) == 0 {
			t.Fatalf("missing %v in:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

// Agent turns with Herdr panes: no pane reservation is attempted while a hold
// is in force, and pressure proven by the reservation's own reading enters it.
func TestLiveAdmissionPanePathHonoursPressureHold(t *testing.T) {
	setup := func(t *testing.T) (*liveAdmission, *pressureClock, *int) {
		a, clock, _ := pressureFixture(t, 2)
		a.paneLimit = 3
		var e error
		if a.owner, e = scheduler.CurrentOwner(); e != nil {
			t.Fatal(e)
		}
		if a.birth, e = processowner.Birth(a.owner.PID); e != nil {
			t.Fatal(e)
		}
		if a.panes, e = scheduler.OpenPaneLedger(filepath.Join(a.daemon.layout.Root, "panes"), a.paneLimit); e != nil {
			t.Fatal(e)
		}
		inventories := 0
		a.inventory = func(context.Context) (map[string]int, error) { inventories++; return map[string]int{}, nil }
		return a, clock, &inventories
	}
	agent := engine.Dispatch{Config: workflow.Stage{Primitive: workflow.AgentTurn}}
	c := hostguard.DefaultConfig()
	t.Run("hold-before-reserve", func(t *testing.T) {
		a, clock, inventories := setup(t)
		readings := 0
		a.observe = func(context.Context) (hostguard.Sample, error) {
			readings++
			if readings <= 2 {
				return hostSample(c.IO+1, 0.5), nil
			}
			return hostSample(1, 0.5), nil
		}
		if _, e := a.Acquire(context.Background(), agent, execution.Binding{}, engine.Session{ID: "pane-1"}); refusalReason(t, e) != "HostPressureHold" || *inventories != 0 {
			t.Fatal("reserved while held", e, *inventories)
		}
		clock.advance(5 * time.Second)
		release, e := a.Acquire(context.Background(), agent, execution.Binding{}, engine.Session{ID: "pane-2"})
		if e != nil || *inventories != 1 {
			t.Fatal("lift then reserve", e, *inventories)
		}
		if rows, e := a.panes.Snapshot(context.Background()); e != nil || len(rows) != 1 {
			t.Fatal(rows, e)
		}
		if e = release(); e != nil {
			t.Fatal(e)
		}
		assertHostCounts(t, a, 0, 0)
	})
	t.Run("reservation-reading-enters-hold", func(t *testing.T) {
		a, _, inventories := setup(t)
		calls := 0
		a.observe = func(context.Context) (hostguard.Sample, error) {
			calls++
			if calls == 2 { // the pane ledger's own reading
				return hostSample(1, c.Load+1), nil
			}
			return hostSample(1, 0.5), nil
		}
		if _, e := a.Acquire(context.Background(), agent, execution.Binding{}, engine.Session{ID: "pane-3"}); refusalReason(t, e) != "HostPressureHold" || *inventories != 1 || !a.pressureState.hold.Active() {
			t.Fatal(e, *inventories)
		}
		if rows, e := a.panes.Snapshot(context.Background()); e != nil || len(rows) != 0 {
			t.Fatal("reservation leaked", rows, e)
		}
		assertHostCounts(t, a, 0, 0)
	})
}

func TestPressureGuardAndProductionPolicy(t *testing.T) {
	for _, heavy := range []bool{false, true} {
		for _, passed := range []bool{false, true} {
			if CanEnqueueHeavyLease(heavy, passed) != (!heavy || passed) {
				t.Fatal(heavy, passed)
			}
		}
	}
	if productionPressurePolicy() != hostguard.DefaultPolicy() {
		t.Fatal("production live admission must use the requirement defaults")
	}
	for _, p := range []workflow.Primitive{workflow.HostCommand, workflow.VcsCommit, workflow.Integrate} {
		if !hostHeavy(p) {
			t.Fatal(p)
		}
	}
	for _, p := range []workflow.Primitive{workflow.AgentTurn, workflow.Review, workflow.Merge, ""} {
		if hostHeavy(p) {
			t.Fatal(p)
		}
	}
	a := admissionFixture(t, 1)
	bad := hostguard.Policy{}
	a.policy = &bad
	if _, e := a.Acquire(context.Background(), heavyDispatch(workflow.HostCommand), execution.Binding{}, engine.Session{ID: "bad-policy"}); e == nil {
		t.Fatal("invalid pressure policy admitted")
	}
	assertHostCounts(t, a, 0, 0)
}

package controller

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

func admissionFixture(t *testing.T, samples ...hostguard.Sample) (HeavyAdmission, *fakeClock, *[]string, *int) {
	t.Helper()
	ledger, e := scheduler.OpenDynamicLedger(filepath.Join(t.TempDir(), "host"), 2)
	if e != nil {
		t.Fatal(e)
	}
	owner, e := scheduler.CurrentOwner()
	if e != nil {
		t.Fatal(e)
	}
	clock := &fakeClock{now: time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)}
	alerts, reads := &[]string{}, new(int)
	return HeavyAdmission{Ledger: ledger, Owner: owner, Clock: clock, MaxWait: 5 * time.Minute,
		Probe: func(scheduler.Owner) scheduler.Liveness { return scheduler.Unknown },
		Observe: func(context.Context) (hostguard.Sample, error) {
			s := samples[min(*reads, len(samples)-1)]
			*reads++
			return s, nil
		},
		Alert: func(line string) { *alerts = append(*alerts, line) }}, clock, alerts, reads
}

func hostSample(gib, io10, memory10, load float64) hostguard.Sample {
	r := func(v float64) hostguard.Reading { return hostguard.Reading{Value: v, Known: true} }
	return hostguard.Sample{Memory: r(gib * float64(hostguard.GiB)), Swap: r(float64(hostguard.GiB)), IO10: r(io10), Memory10: r(memory10), LoadPerCore: r(load)}
}

// FAE-HOST-003: the hard breaker refuses at once, takes no lease, and is
// reported both as an alert line and to the caller's alert hook.
func TestHeavyAdmissionHardRefusalAlerts(t *testing.T) {
	a, clock, alerts, _ := admissionFixture(t, hostSample(1, 1, 0, 0))
	var hard []hostguard.Sample
	a.Hard = func(s hostguard.Sample) { hard = append(hard, s) }
	start := clock.Now()
	var refused *AdmissionRefused
	if _, e := a.Acquire(context.Background(), "hard"); !errors.As(e, &refused) || refused.Reason != "HostHardLimit" {
		t.Fatal(e)
	}
	if clock.Now() != start || len(hard) != 1 || len(*alerts) != 1 || !strings.Contains((*alerts)[0], "reason=HostHardLimit") {
		t.Fatal("hard refusal backed off or raised no alert", *alerts, hard)
	}
	if pool, _ := a.Ledger.Snapshot(context.Background()); len(pool.Holders)+len(pool.Waiting) != 0 {
		t.Fatal(pool)
	}
}

// FAE-HOST-002, -005, -010: a role-class heavy op backs off on soft pressure
// for at most the policy's checks and then refuses; once pressure was seen the
// hold lifts only on two clear readings a window apart, and the I/O gate needs
// its own two readings before the lease is requested.
func TestHeavyAdmissionRoleBacksOffBoundedThenRefuses(t *testing.T) {
	policy := hostguard.DefaultPolicy()
	a, clock, _, reads := admissionFixture(t, hostSample(64, 1, 0, 4.5))
	a.Class, a.MemoryFloor, a.HostChecks = "role", -1, policy.Checks
	start := clock.Now()
	var timeout *AdmissionTimeout
	if _, e := a.Acquire(context.Background(), "load"); !errors.As(e, &timeout) || timeout.Stage != "pressure" || !strings.Contains(timeout.Detail, "load_per_core=4.50") {
		t.Fatal(e)
	}
	if *reads != policy.Checks+1 || clock.Now().Sub(start) != policy.Interval*time.Duration(policy.Checks) {
		t.Fatal("back-off not bounded to the policy checks", *reads, clock.Now().Sub(start))
	}
	a, clock, _, _ = admissionFixture(t, hostSample(64, 1, 0, 4.5), hostSample(64, 1, 0, 1))
	a.Class, a.MemoryFloor = "role", -1
	start = clock.Now()
	release, e := a.Acquire(context.Background(), "lifted")
	if e != nil {
		t.Fatal(e)
	}
	if clock.Now().Sub(start) < policy.Window+policy.Interval {
		t.Fatal("pressure hold lifted without two clear readings a window apart", clock.Now().Sub(start))
	}
	pool, _ := a.Ledger.Snapshot(context.Background())
	if len(pool.Holders) != 1 || pool.Holders[0].Class != 0 {
		t.Fatal("role lease", pool)
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
}

// psiSample is a host reading with every PSI full average set.
func psiSample(gib, load, io10, io60, memory10, memory60 float64) hostguard.Sample {
	s := hostSample(gib, io10, memory10, load)
	s.IO60, s.Memory60 = hostguard.Reading{Value: io60, Known: true}, hostguard.Reading{Value: memory60, Known: true}
	return s
}

// recordedLine returns the first alert line that starts with prefix and
// checks that it carries each key=value field exactly.
func recordedLine(t *testing.T, alerts []string, prefix string, fields ...string) string {
	t.Helper()
	for _, line := range alerts {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		have := map[string]bool{}
		for _, f := range strings.Fields(line) {
			have[f] = true
		}
		for _, f := range fields {
			if !have[f] {
				t.Fatalf("%q lacks %s:\n%s", prefix, f, line)
			}
		}
		return line
	}
	t.Fatalf("no line starts with %q: %q", prefix, alerts)
	return ""
}

// FAE-HOST-005: a reading held for pressure records its avg10 and avg60 PSI
// values and the direction of each since the previous reading, and the
// reading that lifts the hold is recorded the same way.
func TestHeavyAdmissionPressureWaitRecordsAvg60AndDirection(t *testing.T) {
	policy := hostguard.DefaultPolicy()
	clear := psiSample(64, 1, 1, 1.5, 0.25, 0.75)
	samples := []hostguard.Sample{
		psiSample(64, 4.5, 1, 2, 0.5, 0.25),   // 0 s: the hold is entered and recorded at once
		psiSample(64, 4.5, 2, 1.5, 0.5, 0.75), // 3 s: recorded wait slice
	}
	for range 10 { // 6 s .. 33 s: the clearing chain
		samples = append(samples, clear)
	}
	samples = append(samples, psiSample(64, 1, 0.5, 1.5, 0.25, 0.5)) // 36 s: lifts the hold
	a, _, alerts, _ := admissionFixture(t, samples...)
	a.Class, a.MemoryFloor, a.Slice = "role", -1, policy.Interval
	release, e := a.Acquire(context.Background(), "avg60")
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	recordedLine(t, *alerts, "gate waiting stage=pressure id=avg60 refused: HostPressure ",
		"load_per_core=4.50", "io_full_avg10=1.00", "io_full_avg60=2.00", "io_avg10_dir=unknown", "memory_avg60_dir=unknown")
	recordedLine(t, (*alerts)[1:], "gate waiting stage=pressure id=avg60 refused: HostPressure ",
		"load_per_core=4.50", "io_full_avg10=2.00", "io_full_avg60=1.50", "io_avg10_dir=rising", "io_avg60_dir=falling",
		"memory_full_avg10=0.50", "memory_full_avg60=0.75", "memory_avg10_dir=steady", "memory_avg60_dir=rising")
	recordedLine(t, *alerts, "gate lifted stage=pressure id=avg60 proceed: PressureHoldLifted ",
		"load_per_core=1.00", "io_full_avg10=0.50", "io_full_avg60=1.50", "io_avg10_dir=falling", "io_avg60_dir=steady",
		"memory_full_avg10=0.25", "memory_full_avg60=0.50", "memory_avg10_dir=steady", "memory_avg60_dir=falling")
}

// FAE-HOST-005: every other reading the controller path records (an I/O gate
// wait and its lift, a memory-floor wait and its timeout, a hard refusal, a
// reading that sends the admission back from under its lease) carries avg10,
// avg60 and both directions, with the values of that reading.
func TestHeavyAdmissionEveryRecordedReadingCarriesAvg60AndDirection(t *testing.T) {
	policy := hostguard.DefaultPolicy()
	t.Run("io", func(t *testing.T) {
		samples := []hostguard.Sample{psiSample(64, 1, 7, 6, 0, 0), psiSample(64, 1, 8, 5, 0, 0)}
		for range 10 {
			samples = append(samples, psiSample(64, 1, 1, 4, 0, 0))
		}
		samples = append(samples, psiSample(64, 1, 0.5, 3, 0, 0))
		a, _, alerts, _ := admissionFixture(t, samples...)
		a.MemoryFloor, a.Slice = -1, policy.Interval
		release, e := a.Acquire(context.Background(), "io")
		if e != nil {
			t.Fatal(e)
		}
		defer release()
		// The reading that starts the hold, at once, then the slice's.
		recordedLine(t, *alerts, "gate waiting stage=io id=io refused: IOPressure ", "io_full_avg10=7.00", "io_full_avg60=6.00", "io_avg10_dir=unknown")
		recordedLine(t, (*alerts)[1:], "gate waiting stage=io id=io refused: IOPressure ",
			"io_full_avg10=8.00", "io_full_avg60=5.00", "io_avg10_dir=rising", "io_avg60_dir=falling",
			"memory_full_avg10=0.00", "memory_full_avg60=0.00", "memory_avg10_dir=steady", "memory_avg60_dir=steady")
		recordedLine(t, *alerts, "gate lifted stage=io id=io proceed: IOGateClear ",
			"io_full_avg10=0.50", "io_full_avg60=3.00", "io_avg10_dir=falling", "io_avg60_dir=falling")
	})
	t.Run("memory", func(t *testing.T) {
		a, _, alerts, _ := admissionFixture(t, psiSample(11.5, 1, 1, 2, 0.5, 1), psiSample(11.5, 1, 1, 3, 0.25, 1))
		a.Slice, a.MaxWait = policy.Interval, 2*policy.Interval
		var timeout *AdmissionTimeout
		if _, e := a.Acquire(context.Background(), "memory"); !errors.As(e, &timeout) || timeout.Stage != "memory" {
			t.Fatal(e)
		}
		// The reading that starts the hold, at once, then the slice's.
		recordedLine(t, *alerts, "gate waiting stage=memory id=memory ", "io_full_avg60=2.00", "io_avg60_dir=unknown", "memory_full_avg10=0.50", "memory_avg10_dir=unknown")
		fields := []string{"memory_available=12348030976", "io_full_avg10=1.00", "io_full_avg60=3.00", "io_avg10_dir=steady", "io_avg60_dir=rising",
			"memory_full_avg10=0.25", "memory_full_avg60=1.00", "memory_avg10_dir=falling", "memory_avg60_dir=steady"}
		recordedLine(t, (*alerts)[1:], "gate waiting stage=memory id=memory ", fields...)
		recordedLine(t, *alerts, "gate timeout stage=memory id=memory ", "io_avg10_dir=steady", "io_avg60_dir=steady", "memory_avg10_dir=steady", "memory_avg60_dir=steady")
	})
	t.Run("hard", func(t *testing.T) {
		a, _, alerts, _ := admissionFixture(t, psiSample(1, 1, 3, 2, 0.5, 0.25))
		var refused *AdmissionRefused
		if _, e := a.Acquire(context.Background(), "hard"); !errors.As(e, &refused) {
			t.Fatal(e)
		}
		recordedLine(t, *alerts, "gate refused stage=host id=hard reason=HostHardLimit ",
			"memory_available=1073741824", "swap_total=1073741824", "io_full_avg10=3.00", "io_full_avg60=2.00", "io_avg10_dir=unknown", "io_avg60_dir=unknown",
			"memory_full_avg10=0.50", "memory_full_avg60=0.25", "memory_avg10_dir=unknown", "memory_avg60_dir=unknown")
	})
	t.Run("under-lease", func(t *testing.T) {
		quiet := psiSample(64, 1, 1, 1, 0, 0)
		samples := []hostguard.Sample{}
		for range 11 { // 0 s .. 30 s: the I/O gate's two readings
			samples = append(samples, quiet)
		}
		samples = append(samples, psiSample(64, 9, 2, 1, 0.5, 0.5)) // under the granted lease
		for range 40 {
			samples = append(samples, quiet)
		}
		a, _, alerts, _ := admissionFixture(t, samples...)
		a.Slice = policy.Interval
		release, e := a.Acquire(context.Background(), "lease")
		if e != nil {
			t.Fatal(e)
		}
		defer release()
		recordedLine(t, *alerts, "gate waiting stage=pressure id=lease refused: HostPressure under the granted lease ",
			"load_per_core=9.00", "io_full_avg10=2.00", "io_full_avg60=1.00", "io_avg10_dir=rising", "io_avg60_dir=steady",
			"memory_full_avg10=0.50", "memory_full_avg60=0.50", "memory_avg10_dir=rising", "memory_avg60_dir=rising")
		recordedLine(t, *alerts, "gate lifted stage=pressure id=lease proceed: PressureHoldLifted ", "load_per_core=1.00", "io_avg10_dir=steady")
	})
}

// FAE-HOST-005 on the controller path: the reading that starts a hold (soft
// pressure, I/O pressure at the gate, the landing floor) is recorded at once,
// and the reading that lifts the hold is recorded too, though the whole wait
// is shorter than one slice (the default minute). Before, a wait shorter than
// a slice recorded no reading at all. The readings in between stay unrecorded
// (one line per slice), and the clear readings of the routine I/O window add
// no line.
func TestHeavyAdmissionRecordsAHoldShorterThanASlice(t *testing.T) {
	clear := psiSample(64, 1, 1, 1, 0, 0)
	for _, c := range []struct {
		name, waiting, lifted string
		first                 hostguard.Sample
		fields                []string
	}{
		{"pressure", "gate waiting stage=pressure id=pressure refused: HostPressure ", "gate lifted stage=pressure id=pressure proceed: PressureHoldLifted ",
			psiSample(64, 4.5, 2, 1.5, 0.5, 0.75), []string{"load_per_core=4.50", "io_full_avg10=2.00", "io_full_avg60=1.50", "io_avg10_dir=unknown", "memory_full_avg10=0.50", "memory_full_avg60=0.75", "memory_avg60_dir=unknown"}},
		{"io", "gate waiting stage=io id=io refused: IOPressure ", "gate lifted stage=io id=io proceed: IOGateClear ",
			psiSample(64, 1, 7, 6, 0, 0), []string{"io_full_avg10=7.00", "io_full_avg60=6.00", "io_avg10_dir=unknown", "io_avg60_dir=unknown"}},
		{"memory", "gate waiting stage=memory id=memory memory available ", "",
			psiSample(11.5, 1, 1, 1, 0, 0), []string{"memory_available=12348030976", "io_full_avg10=1.00", "io_avg10_dir=unknown"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			samples := []hostguard.Sample{c.first}
			for range 20 {
				samples = append(samples, clear)
			}
			a, clock, alerts, _ := admissionFixture(t, samples...)
			start := clock.Now()
			release, e := a.Acquire(context.Background(), c.name)
			if e != nil {
				t.Fatal(e)
			}
			defer release()
			if waited := clock.Now().Sub(start); waited >= time.Minute {
				t.Fatal("the wait was not shorter than one slice", waited)
			}
			recordedLine(t, *alerts, c.waiting, c.fields...)
			want := 1
			if c.lifted != "" {
				recordedLine(t, *alerts, c.lifted, "load_per_core=1.00", "io_full_avg10=1.00", "io_avg10_dir=steady")
				want = 2
			}
			if len(*alerts) != want {
				t.Fatalf("want the hold's first reading and its lift only, got %q", *alerts)
			}
		})
	}
}

// FAE-HOST-001: pressure seen under the granted lease releases it and returns
// to the bounded host wait; the lease is never held through a back-off.
func TestHeavyAdmissionPressureUnderLeaseReleases(t *testing.T) {
	quiet, loaded := hostSample(64, 1, 0, 1), hostSample(64, 1, 0, 9)
	// Readings 0..10 pass the I/O window; reading 11 is under the lease.
	samples := []hostguard.Sample{}
	for range 11 {
		samples = append(samples, quiet)
	}
	samples = append(samples, loaded)
	for range 40 {
		samples = append(samples, quiet)
	}
	a, _, _, reads := admissionFixture(t, samples...)
	release, e := a.Acquire(context.Background(), "under-lease")
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	audit, _ := a.Ledger.Audit(context.Background())
	granted := 0
	for _, entry := range audit {
		if entry.ID == "under-lease" && entry.Action == "granted" {
			granted++
		}
	}
	if granted != 2 || *reads < 14 {
		t.Fatal("pressure under the lease did not release it and wait again", granted, *reads)
	}
}

// FAE-HOST-031: every admission access reconciles a proven-dead owner's entry.
func TestHeavyAdmissionReclaimsDeadOwner(t *testing.T) {
	a, _, _, _ := admissionFixture(t, hostSample(64, 1, 0, 1))
	dead := scheduler.Owner{PID: 4242, Started: 77}
	ctx := context.Background()
	if e := a.Ledger.Enqueue(ctx, scheduler.Request{ID: "crashed", Owner: dead, Weight: 2, Queued: time.Now().UTC()}); e != nil {
		t.Fatal(e)
	}
	if _, _, e := a.Ledger.Advance(ctx, time.Now().UTC(), map[scheduler.Owner]scheduler.Liveness{}); e != nil {
		t.Fatal(e)
	}
	var timeout *AdmissionTimeout
	if _, e := a.Acquire(ctx, "ambiguous"); !errors.As(e, &timeout) || timeout.Stage != "lease" {
		t.Fatal("ambiguous owner reclaimed", e)
	}
	a.Probe = func(o scheduler.Owner) scheduler.Liveness {
		if o == dead {
			return scheduler.Dead
		}
		return scheduler.Unknown
	}
	release, e := a.Acquire(ctx, "after-death")
	if e != nil {
		t.Fatal("dead owner kept its capacity", e)
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
}

package hostguard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// fakeClock advances only when Sleep is called, so 30-second windows run
// instantly and deterministically.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	slept   []time.Duration
	onSleep func()
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	hook := c.onSleep
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return ctx.Err()
}

func calm() Sample {
	s := healthy()
	s.IO10, s.IO60 = Reading{1, true}, Reading{1, true}
	return s
}
func withIO(v float64) Sample {
	s := calm()
	s.IO10 = Reading{v, true}
	return s
}
func withLoad(v float64) Sample {
	s := calm()
	s.LoadPerCore = Reading{v, true}
	return s
}
func mustHold(t *testing.T, gate bool) *Hold {
	t.Helper()
	h, err := newHold(DefaultPolicy(), gate)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func expect(t *testing.T, label string, s Step, outcome Outcome, reason string) {
	t.Helper()
	if s.Outcome != outcome || s.Reason != reason {
		t.Fatalf("%s: got %s/%s want %s/%s (%+v)", label, s.Outcome, s.Reason, outcome, reason, s)
	}
}

// FAE-HOST-005 acceptance: a second reading above threshold keeps the hold.
func TestPressureHoldSecondReadingAboveThresholdKeepsHold(t *testing.T) {
	c := DefaultConfig()
	h := mustHold(t, false)
	if h.Active() {
		t.Fatal("hold active before pressure")
	}
	expect(t, "enter", h.Record(epoch, withLoad(c.Load+1), c), OutcomeRefused, "HostPressure")
	if !h.Active() {
		t.Fatal("pressure did not enter hold")
	}
	first := h.Record(epoch.Add(time.Second), calm(), c)
	expect(t, "first below", first, OutcomeWaiting, "HostPressureHold")
	if !first.ChainStart.Equal(epoch.Add(time.Second)) || !first.HeldBefore {
		t.Fatal(first)
	}
	// Second reading 30 s later is above threshold: the hold stays and the chain restarts.
	second := h.Record(epoch.Add(31*time.Second), withIO(c.IO+0.01), c)
	expect(t, "second above", second, OutcomeRefused, "HostPressure")
	if !h.Active() || !second.ChainStart.IsZero() {
		t.Fatal("second reading above threshold lifted or kept chain", second)
	}
	// A reading exactly at the threshold during a hold is not "below" and also
	// keeps the hold, although it would proceed when no hold is in force.
	expect(t, "restart", h.Record(epoch.Add(32*time.Second), calm(), c), OutcomeWaiting, "HostPressureHold")
	expect(t, "at threshold", h.Record(epoch.Add(62*time.Second), withIO(c.IO), c), OutcomeRefused, "HostPressure")
	if !h.Active() {
		t.Fatal("at-threshold reading lifted hold")
	}
	expect(t, "chain", h.Record(epoch.Add(63*time.Second), calm(), c), OutcomeWaiting, "HostPressureHold")
	lift := h.Record(epoch.Add(93*time.Second), calm(), c)
	expect(t, "lift", lift, OutcomeProceed, "PressureHoldLifted")
	if !lift.Lifted || h.Active() {
		t.Fatal("two clear readings 30 s apart did not lift", lift)
	}
	// Once lifted, ordinary admission resumes on a single reading.
	expect(t, "after", h.Record(epoch.Add(94*time.Second), calm(), c), OutcomeProceed, "")
}

// Two readings fewer than 30 s apart never lift the hold or pass the gate.
func TestPressureHoldReadingsUnderThirtySecondsDoNotLift(t *testing.T) {
	c := DefaultConfig()
	for _, gate := range []bool{false, true} {
		h := mustHold(t, gate)
		waiting, lifted := "HostPressureHold", "PressureHoldLifted"
		if gate {
			waiting, lifted = "IOPressureHold", "IOGateClear"
		} else {
			h.Record(epoch, withLoad(c.Load*2), c)
		}
		start := epoch.Add(time.Second)
		expect(t, "first", h.Record(start, calm(), c), OutcomeWaiting, waiting)
		for _, offset := range []time.Duration{3 * time.Second, 15 * time.Second, 30*time.Second - time.Nanosecond} {
			expect(t, "early", h.Record(start.Add(offset), calm(), c), OutcomeWaiting, waiting)
		}
		expect(t, "spaced", h.Record(start.Add(30*time.Second), calm(), c), OutcomeProceed, lifted)
	}
}

// FAE-HOST-002 says a value exactly at a threshold proceeds; FAE-HOST-010 says
// I/O readings must be "below" the gate threshold. Each is implemented as
// written, so the two boundaries deliberately differ.
func TestThresholdBoundarySemantics(t *testing.T) {
	c := DefaultConfig()
	soft := mustHold(t, false)
	if l, _ := SoftLevel(withIO(c.IO), c); l != LevelAtThreshold {
		t.Fatal(l)
	}
	expect(t, "soft at", soft.Record(epoch, withIO(c.IO), c), OutcomeProceed, "")
	expect(t, "soft above", soft.Record(epoch.Add(time.Second), withIO(c.IO+0.001), c), OutcomeRefused, "HostPressure")
	for _, s := range []Sample{withLoad(c.Load), func() Sample { s := calm(); s.Memory10 = Reading{c.MemoryPressure, true}; return s }(), func() Sample { s := calm(); s.Memory = Reading{float64(c.Soft), true}; return s }()} {
		if l, _ := SoftLevel(s, c); l != LevelAtThreshold || !Admit(s, c, 0, true).Allowed {
			t.Fatal("exact soft threshold must proceed", s)
		}
	}
	p := DefaultPolicy()
	if l, _ := IOGateLevel(withIO(p.IOGate), p.IOGate); l != LevelAtThreshold {
		t.Fatal(l)
	}
	if l, _ := IOGateLevel(withIO(p.IOGate-0.01), p.IOGate); l != LevelClear {
		t.Fatal(l)
	}
	gate := mustHold(t, true)
	expect(t, "gate first", gate.Record(epoch, withIO(p.IOGate-0.01), c), OutcomeWaiting, "IOPressureHold")
	expect(t, "gate at", gate.Record(epoch.Add(30*time.Second), withIO(p.IOGate), c), OutcomeRefused, "IOPressure")
	expect(t, "gate above", gate.Record(epoch.Add(31*time.Second), withIO(p.IOGate+1), c), OutcomeRefused, "IOPressure")
}

// Unreadable soft telemetry proceeds and records the gap (FAE-HOST-004); it
// neither lifts nor resets an open chain.
func TestUnknownTelemetryProceedsWithGap(t *testing.T) {
	c := DefaultConfig()
	h := mustHold(t, false)
	h.Record(epoch, withLoad(c.Load+1), c)
	expect(t, "chain", h.Record(epoch.Add(time.Second), calm(), c), OutcomeWaiting, "HostPressureHold")
	unknown := h.Record(epoch.Add(2*time.Second), Sample{}, c)
	expect(t, "unknown", unknown, OutcomeProceed, "TelemetryGap")
	if !unknown.Gap || unknown.Level != LevelUnknown || unknown.Lifted || !h.Active() {
		t.Fatal("unknown telemetry must proceed with gap without lifting", unknown)
	}
	expect(t, "chain survives", h.Record(epoch.Add(31*time.Second), calm(), c), OutcomeProceed, "PressureHoldLifted")

	// Partial telemetry (no PSI, as on macOS) still decides on readable values.
	partial := calm()
	partial.IO10, partial.IO60, partial.Memory10, partial.Memory60 = Reading{}, Reading{}, Reading{}, Reading{}
	step := mustHold(t, false).Record(epoch, partial, c)
	expect(t, "partial", step, OutcomeProceed, "TelemetryGap")
	if !step.Gap || step.Level != LevelClear {
		t.Fatal(step)
	}
	gate := mustHold(t, true)
	gstep := gate.Record(epoch, partial, c)
	expect(t, "gate unknown", gstep, OutcomeProceed, "TelemetryGap")
	if !gstep.Gap {
		t.Fatal(gstep)
	}
	if got := Missing(partial); len(got) != 4 || got[0] != "io_full_avg10" {
		t.Fatal(got)
	}
}

func TestPressureReadingsRecordAveragesAndDirection(t *testing.T) {
	c := DefaultConfig()
	h := mustHold(t, true)
	s := calm()
	s.IO10, s.IO60, s.Memory10, s.Memory60 = Reading{2, true}, Reading{1, true}, Reading{0.5, true}, Reading{0.5, true}
	first := h.Record(epoch, s, c)
	if first.Reading.IO.Avg10.Value != 2 || first.Reading.IO.Avg60.Value != 1 || first.Reading.IO.Dir10 != DirectionUnknown || !first.Reading.At.Equal(epoch) {
		t.Fatal(first.Reading)
	}
	s.IO10, s.IO60, s.Memory10 = Reading{1, true}, Reading{1.5, true}, Reading{0.5, true}
	s.Memory60 = Reading{}
	second := h.Record(epoch.Add(3*time.Second), s, c)
	r := second.Reading
	if r.IO.Dir10 != DirectionFalling || r.IO.Dir60 != DirectionRising || r.Memory.Dir10 != DirectionSteady || r.Memory.Dir60 != DirectionUnknown || r.IO.Avg10.Value != 1 || r.IO.Avg60.Value != 1.5 {
		t.Fatal(r)
	}
}

// A chain whose consecutive readings are further apart than MaxGap restarts;
// after the gate passes, its unbroken chain admits later commands at once.
func TestIOGateChainContinuity(t *testing.T) {
	c := DefaultConfig()
	g := mustHold(t, true)
	g.Record(epoch, calm(), c)
	expect(t, "stale", g.Record(epoch.Add(61*time.Second), calm(), c), OutcomeWaiting, "IOPressureHold")
	expect(t, "pass", g.Record(epoch.Add(91*time.Second), calm(), c), OutcomeProceed, "IOGateClear")
	expect(t, "retained", g.Record(epoch.Add(100*time.Second), calm(), c), OutcomeProceed, "IOGateClear")
	expect(t, "pressure", g.Record(epoch.Add(101*time.Second), withIO(9), c), OutcomeRefused, "IOPressure")
	expect(t, "restart", g.Record(epoch.Add(102*time.Second), calm(), c), OutcomeWaiting, "IOPressureHold")
	expect(t, "silence", g.Record(epoch.Add(200*time.Second), calm(), c), OutcomeWaiting, "IOPressureHold")
	// A reading older than the chain's latest neither extends nor lifts it.
	expect(t, "out of order", g.Record(epoch.Add(150*time.Second), calm(), c), OutcomeWaiting, "IOPressureHold")
	expect(t, "pass again", g.Record(epoch.Add(230*time.Second), calm(), c), OutcomeProceed, "IOGateClear")
}

func waitWith(t *testing.T, ctx context.Context, clock Clock, p Policy, h *Hold, next func(int) Sample) (Step, int, error) {
	t.Helper()
	return Wait(ctx, clock, p, func(ctx context.Context, n int) (Step, error) {
		return h.Record(clock.Now(), next(n), DefaultConfig()), nil
	})
}

// Back-off is bounded (FAE-HOST-002: 10 re-checks at 3 s) and its final
// outcome distinguishes a pending second reading from proven pressure.
func TestWaitBoundedDistinguishesWaitingFromRefused(t *testing.T) {
	ctx := context.Background()
	p := DefaultPolicy()
	clock := &fakeClock{now: epoch}
	step, n, err := waitWith(t, ctx, clock, p, mustHold(t, true), func(int) Sample { return withIO(50) })
	if err != nil || n != p.Checks+1 || step.Outcome != OutcomeRefused || step.Reason != "IOPressure" || len(clock.slept) != p.Checks {
		t.Fatal("sustained pressure", step, n, err, clock.slept)
	}
	for _, d := range clock.slept {
		if d != 3*time.Second {
			t.Fatal(clock.slept)
		}
	}
	// A fresh clear chain completes its 30 s window on the last re-check.
	clock = &fakeClock{now: epoch}
	step, n, err = waitWith(t, ctx, clock, p, mustHold(t, true), func(int) Sample { return calm() })
	if err != nil || n != p.Checks+1 || step.Outcome != OutcomeProceed || !step.Lifted || clock.Now().Sub(epoch) != 30*time.Second {
		t.Fatal("clear chain", step, n, err)
	}
	// With a shorter bound the same clear chain ends Waiting, not Refused.
	short := p
	short.Checks = 4
	clock = &fakeClock{now: epoch}
	step, n, err = waitWith(t, ctx, clock, short, mustHold(t, true), func(int) Sample { return calm() })
	if err != nil || n != 5 || step.Outcome != OutcomeWaiting || step.Reason != "IOPressureHold" {
		t.Fatal("waiting", step, n, err)
	}
	// Recovery: pressure first, then calm. The chain restarts after the last
	// pressured reading and lifts 30 s later.
	clock = &fakeClock{now: epoch}
	hold := mustHold(t, false)
	step, n, err = waitWith(t, ctx, clock, short, hold, func(n int) Sample {
		if n < 2 {
			return withLoad(9)
		}
		return calm()
	})
	if err != nil || step.Outcome != OutcomeWaiting || !hold.Active() {
		t.Fatal("recovery bound", step, n, err)
	}
	step, n, err = waitWith(t, ctx, clock, p, hold, func(int) Sample { return calm() })
	if err != nil || step.Outcome != OutcomeProceed || !step.Lifted || hold.Active() || clock.Now().Sub(epoch) != 6*time.Second+30*time.Second {
		t.Fatal("recovery lift", step, n, err, clock.Now().Sub(epoch))
	}
	// A stop outcome (hard breaker) returns at once without back-off.
	clock = &fakeClock{now: epoch}
	step, n, err = Wait(ctx, clock, p, func(context.Context, int) (Step, error) {
		return Step{Outcome: OutcomeStop, Reason: "HostHardLimit"}, nil
	})
	if err != nil || n != 1 || step.Outcome != OutcomeStop || len(clock.slept) != 0 {
		t.Fatal("stop", step, n, err)
	}
	// Sampling errors end the wait.
	boom := errors.New("telemetry failed")
	if _, _, err = Wait(ctx, clock, p, func(context.Context, int) (Step, error) { return Step{}, boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, _, err = Wait(ctx, clock, Policy{}, func(context.Context, int) (Step, error) { return Step{}, nil }); err == nil {
		t.Fatal("invalid policy accepted")
	}
}

func TestWaitCancellationReturnsPromptly(t *testing.T) {
	// Real clock: cancellation during a long back-off sleep returns at once.
	p := DefaultPolicy()
	p.Interval, p.MaxGap = time.Hour, 2*time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	h := mustHold(t, true)
	readings := 0
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	started := time.Now()
	step, n, err := Wait(ctx, SystemClock{}, p, func(ctx context.Context, _ int) (Step, error) {
		readings++
		return h.Record(time.Now(), calm(), DefaultConfig()), nil
	})
	if !errors.Is(err, context.Canceled) || time.Since(started) > 5*time.Second || readings != 1 || n != 1 || step.Outcome != OutcomeWaiting {
		t.Fatal("cancel during sleep", step, n, err, time.Since(started))
	}
	// Injected clock: cancellation observed between readings stops sampling.
	ctx, cancel = context.WithCancel(context.Background())
	clock := &fakeClock{now: epoch}
	clock.onSleep = cancel
	readings = 0
	_, _, err = waitWith(t, ctx, clock, DefaultPolicy(), mustHold(t, true), func(int) Sample { readings++; return calm() })
	if !errors.Is(err, context.Canceled) || readings != 1 {
		t.Fatal("cancel between readings", err, readings)
	}
	// An already-cancelled context takes no reading.
	readings = 0
	if _, n, err = waitWith(t, ctx, clock, DefaultPolicy(), mustHold(t, true), func(int) Sample { readings++; return calm() }); !errors.Is(err, context.Canceled) || readings != 0 || n != 0 {
		t.Fatal("pre-cancelled", err, readings)
	}
}

func TestHoldPolicyValidationAndGuard(t *testing.T) {
	if !ValidPolicy(DefaultPolicy()) {
		t.Fatal("default policy invalid")
	}
	if p := DefaultPolicy(); p.Window != 30*time.Second || p.Interval != 3*time.Second || p.Checks != 10 || p.IOGate != 5 || p.MaxGap != time.Minute {
		t.Fatal("requirement defaults changed", p)
	}
	for _, mutate := range []func(*Policy){func(p *Policy) { p.Window = 0 }, func(p *Policy) { p.Interval = 0 }, func(p *Policy) { p.MaxGap = p.Interval - 1 }, func(p *Policy) { p.Checks = -1 }, func(p *Policy) { p.IOGate = -1 }, func(p *Policy) { p.IOGate = 101 }} {
		p := DefaultPolicy()
		mutate(&p)
		if ValidPolicy(p) {
			t.Fatal(p)
		}
		if _, err := NewIOGate(p); err == nil {
			t.Fatal("invalid gate policy accepted", p)
		}
		if _, err := NewPressureHold(p); err == nil {
			t.Fatal("invalid hold policy accepted", p)
		}
	}
	for i := 0; i < 16; i++ {
		a, b, c, d := i&1 != 0, i&2 != 0, i&4 != 0, i&8 != 0
		if CanLiftPressureHold(a, b, c, d) != (a && b && c && d) {
			t.Fatal(a, b, c, d)
		}
	}
}

// Admit's HostPressure refusal and a hold-entering reading share SoftLevel.
func TestSoftLevelMatchesAdmitHostPressure(t *testing.T) {
	c := DefaultConfig()
	values := []float64{0, c.Load - 0.5, c.Load, c.Load + 0.5, c.IO, c.IO + 1, c.MemoryPressure, c.MemoryPressure + 1}
	for _, io := range values {
		for _, mem := range values {
			for _, load := range values {
				s := calm()
				s.IO10, s.Memory10, s.LoadPerCore = Reading{io, true}, Reading{mem, true}, Reading{load, true}
				level, _ := SoftLevel(s, c)
				if (Admit(s, c, 0, true).Reason == "HostPressure") != (level == LevelPressure) {
					t.Fatal(s, level)
				}
			}
		}
	}
}

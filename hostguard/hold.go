package hostguard

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Clock is the time source of pressure holds and the host-heavy I/O gate.
// Production uses SystemClock; tests inject a deterministic clock, so no test
// needs a real 30-second wait.
type Clock interface {
	Now() time.Time
	// Sleep waits for d or until ctx ends, returning ctx.Err() on cancellation.
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the wall clock. Sleep returns promptly on cancellation.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Policy bounds pressure holds (FAE-HOST-002, -005) and the host-heavy I/O
// gate (FAE-HOST-010).
type Policy struct {
	// Window is the minimum spacing of the two clearing readings.
	Window time.Duration
	// MaxGap is the longest silence between consecutive readings of one
	// clearing chain; a longer silence restarts the chain. The requirement
	// bounds only the minimum spacing; this is a deliberate stricter choice so
	// that an old reading never vouches for an unobserved interval.
	MaxGap time.Duration
	// Interval and Checks bound back-off: the first reading plus at most Checks
	// re-checks, each Interval after the previous one (default 10 at 3 s, so a
	// fresh chain can complete its 30 s window within one bounded wait).
	Interval time.Duration
	Checks   int
	// IOGate is the I/O pressure full avg10 ceiling for host-heavy commands.
	IOGate float64
}

// DefaultPolicy returns the requirement defaults.
func DefaultPolicy() Policy {
	return Policy{Window: 30 * time.Second, MaxGap: 60 * time.Second, Interval: 3 * time.Second, Checks: 10, IOGate: 5}
}

// ValidPolicy rejects unbounded or unsatisfiable hold settings.
func ValidPolicy(p Policy) bool {
	return p.Window > 0 && p.Interval > 0 && p.MaxGap >= p.Interval && p.Checks >= 0 && p.Checks <= 10000 && known(Reading{p.IOGate, true}) && p.IOGate <= 100
}

// Level classifies one reading against the thresholds of one hold.
type Level string

const (
	// LevelClear: every readable value is strictly inside its threshold; the
	// reading counts toward lifting a hold.
	LevelClear Level = "clear"
	// LevelAtThreshold: no readable value is beyond its threshold, but one sits
	// exactly on it. It proceeds when no hold is in force (FAE-HOST-002) but is
	// not "below threshold", so it never lifts a hold (FAE-HOST-005).
	LevelAtThreshold Level = "at-threshold"
	// LevelPressure: a readable value is beyond its threshold.
	LevelPressure Level = "pressure"
	// LevelUnknown: no relevant value is readable. The guard proceeds and
	// records the gap (FAE-HOST-004).
	LevelUnknown Level = "unknown"
)

// Outcome is the admission consequence of one reading.
type Outcome string

const (
	// OutcomeProceed: admission may continue (check Step.Gap for a telemetry gap).
	OutcomeProceed Outcome = "proceed"
	// OutcomeWaiting: the reading qualifies, but the second qualifying reading
	// at least Window later is still due. Back off and read again.
	OutcomeWaiting Outcome = "waiting"
	// OutcomeRefused: the reading shows pressure, or sits at the threshold
	// while a hold is in force. The hold is kept and its chain restarts.
	OutcomeRefused Outcome = "refused"
	// OutcomeStop: refuse immediately without back-off. Holds never produce it;
	// callers use it for the hard circuit breaker (FAE-HOST-003) and other
	// non-pressure refusals.
	OutcomeStop Outcome = "stop"
)

// Direction is the movement of a pressure average since the previous reading
// recorded by the same hold.
type Direction string

const (
	DirectionUnknown Direction = "unknown"
	DirectionRising  Direction = "rising"
	DirectionFalling Direction = "falling"
	DirectionSteady  Direction = "steady"
)

// Trend is one PSI resource's "full" averages with their directions.
type Trend struct {
	Avg10, Avg60 Reading
	Dir10, Dir60 Direction
}

// Observation is the audit record of one reading (FAE-HOST-005).
type Observation struct {
	At         time.Time
	Sample     Sample
	IO, Memory Trend
}

// Step is the result of recording one reading.
type Step struct {
	Outcome Outcome
	Level   Level
	// Reason names the cause: HostPressure/IOPressure (refused),
	// HostPressureHold/IOPressureHold (waiting), PressureHoldLifted/IOGateClear
	// (two-reading rule satisfied), TelemetryGap (proceeding on unreadable
	// telemetry), or a caller-supplied stop reason. Empty for an ordinary
	// proceed without any hold involvement.
	Reason string
	// Gap reports unreadable telemetry relevant to this hold (FAE-HOST-004).
	Gap bool
	// HeldBefore reports that a hold (or the always-on I/O gate) was in force
	// when the reading was taken; Lifted that this reading satisfied the
	// two-reading rule.
	HeldBefore, Lifted bool
	// ChainStart is the first qualifying reading of the current clearing chain
	// after this reading, or zero when none is open.
	ChainStart time.Time
	Reading    Observation
}

// SoftLevel classifies the FAE-HOST-002 soft metrics: memory available against
// the soft floor, I/O and memory pressure full avg10 and load per core against
// their ceilings. A value exactly at a threshold is LevelAtThreshold. Unreadable
// metrics are skipped and reported as a gap; when none is readable the level is
// LevelUnknown.
func SoftLevel(s Sample, c Config) (level Level, gap bool) {
	readable, at, over := 0, false, false
	check := func(r Reading, limit float64, floor bool) {
		if !known(r) {
			gap = true
			return
		}
		readable++
		switch {
		case (floor && r.Value < limit) || (!floor && r.Value > limit):
			over = true
		case r.Value == limit:
			at = true
		}
	}
	check(s.Memory, float64(c.Soft), true)
	check(s.IO10, c.IO, false)
	check(s.Memory10, c.MemoryPressure, false)
	check(s.LoadPerCore, c.Load, false)
	switch {
	case over:
		return LevelPressure, gap
	case readable == 0:
		return LevelUnknown, gap
	case at:
		return LevelAtThreshold, gap
	}
	return LevelClear, gap
}

// IOGateLevel classifies I/O pressure full avg10 for the host-heavy gate.
// FAE-HOST-010 requires readings "below" the threshold, so a reading exactly at
// it does not qualify. This intentionally differs from FAE-HOST-002, where a
// value exactly at a soft threshold proceeds (see SoftLevel).
func IOGateLevel(s Sample, threshold float64) (Level, bool) {
	switch {
	case !known(s.IO10):
		return LevelUnknown, true
	case s.IO10.Value < threshold:
		return LevelClear, false
	case s.IO10.Value == threshold:
		return LevelAtThreshold, false
	}
	return LevelPressure, false
}

// Hold is the hysteresis state shared by the admissions of one guard. A
// pressure hold is entered by a soft-pressure reading and lifted only by two
// strictly-below-threshold readings at least Window apart with no
// disqualifying reading and no silence longer than MaxGap between them
// (FAE-HOST-005). The I/O gate is a hold that is always in force: every
// host-heavy admission needs such a chain (FAE-HOST-010). After the gate
// passes, its chain is retained, so a later command passes on one fresh clear
// reading only while the chain stays unbroken.
//
// Hold performs no I/O and reads no clock: the caller supplies reading times.
type Hold struct {
	mu          sync.Mutex
	policy      Policy
	gate        bool
	active      bool
	first, last time.Time
	prev        Sample
	seen        bool
}

// NewPressureHold returns an inactive FAE-HOST-002/-005 soft pressure hold.
func NewPressureHold(p Policy) (*Hold, error) { return newHold(p, false) }

// NewIOGate returns the FAE-HOST-010 host-heavy I/O gate.
func NewIOGate(p Policy) (*Hold, error) { return newHold(p, true) }

func newHold(p Policy, gate bool) (*Hold, error) {
	if !ValidPolicy(p) {
		return nil, errors.New("invalid pressure hold policy")
	}
	return &Hold{policy: p, gate: gate}, nil
}

// Active reports whether a soft pressure hold is in force. The I/O gate is
// always in force and reports true.
func (h *Hold) Active() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gate || h.active
}

// Record classifies one reading taken at the given time and updates the hold.
// c supplies the soft thresholds; the I/O gate uses Policy.IOGate instead.
func (h *Hold) Record(at time.Time, s Sample, c Config) Step {
	h.mu.Lock()
	defer h.mu.Unlock()
	level, gap := SoftLevel(s, c)
	refused, waiting, lifted := "HostPressure", "HostPressureHold", "PressureHoldLifted"
	if h.gate {
		level, gap = IOGateLevel(s, h.policy.IOGate)
		refused, waiting, lifted = "IOPressure", "IOPressureHold", "IOGateClear"
	}
	step := Step{Level: level, Gap: gap, HeldBefore: h.gate || h.active, Reading: h.observe(at, s)}
	switch {
	case level == LevelUnknown:
		// Unreadable soft telemetry proceeds and records the gap. It proves
		// nothing either way, so an open chain neither advances nor resets.
		step.Outcome, step.Reason = OutcomeProceed, "TelemetryGap"
	case !step.HeldBefore && level != LevelPressure:
		// No hold: clear and exactly-at-threshold readings proceed.
		step.Outcome = OutcomeProceed
	default:
		firstBelow := !h.first.IsZero()
		secondBelow := level == LevelClear
		ordered := firstBelow && !at.Before(h.last)
		unbroken := ordered && at.Sub(h.last) <= h.policy.MaxGap
		spaced := ordered && at.Sub(h.first) >= h.policy.Window
		switch {
		case !secondBelow:
			// Pressure enters (or keeps) the hold; pressure or an at-threshold
			// reading while held restarts the chain.
			h.active = !h.gate
			h.first, h.last = time.Time{}, time.Time{}
			step.Outcome, step.Reason = OutcomeRefused, refused
		case CanLiftPressureHold(firstBelow, secondBelow, spaced, unbroken):
			step.Outcome, step.Reason, step.Lifted = OutcomeProceed, lifted, true
			if h.gate {
				h.last = at
			} else {
				h.active = false
				h.first, h.last = time.Time{}, time.Time{}
			}
		case firstBelow && at.Before(h.last):
			// A reading older than the chain's latest cannot extend or lift it.
			step.Outcome, step.Reason = OutcomeWaiting, waiting
		default:
			if !unbroken {
				h.first = at
			}
			h.last = at
			step.Outcome, step.Reason = OutcomeWaiting, waiting
		}
	}
	if h.gate || h.active {
		step.ChainStart = h.first
	}
	if gap && step.Outcome == OutcomeProceed && step.Reason == "" {
		step.Reason = "TelemetryGap"
	}
	return step
}

func (h *Hold) observe(at time.Time, s Sample) Observation {
	o := Observation{At: at, Sample: s,
		IO:     Trend{s.IO10, s.IO60, direction(s.IO10, h.prev.IO10, h.seen), direction(s.IO60, h.prev.IO60, h.seen)},
		Memory: Trend{s.Memory10, s.Memory60, direction(s.Memory10, h.prev.Memory10, h.seen), direction(s.Memory60, h.prev.Memory60, h.seen)},
	}
	h.prev, h.seen = s, true
	return o
}

func direction(current, previous Reading, seen bool) Direction {
	switch {
	case !seen || !known(current) || !known(previous):
		return DirectionUnknown
	case current.Value > previous.Value:
		return DirectionRising
	case current.Value < previous.Value:
		return DirectionFalling
	}
	return DirectionSteady
}

// Wait applies bounded back-off (FAE-HOST-002): it takes a first reading and at
// most p.Checks re-checks, p.Interval apart, stopping at the first step that
// proceeds or must stop at once. When the bound is exhausted it returns the
// last step; its outcome distinguishes a hold still waiting for its second
// reading (OutcomeWaiting) from a refusal on pressure (OutcomeRefused), and the
// caller refuses and parks. Cancellation is honoured before every reading and
// during every sleep. The int result is the number of readings taken.
func Wait(ctx context.Context, clock Clock, p Policy, check func(ctx context.Context, attempt int) (Step, error)) (Step, int, error) {
	if !ValidPolicy(p) || clock == nil || check == nil {
		return Step{}, 0, errors.New("invalid pressure wait")
	}
	var last Step
	for n := 0; n <= p.Checks; n++ {
		if n > 0 {
			if e := clock.Sleep(ctx, p.Interval); e != nil {
				return last, n, e
			}
		}
		if e := ctx.Err(); e != nil {
			return last, n, e
		}
		step, e := check(ctx, n)
		if e != nil {
			return step, n + 1, e
		}
		last = step
		if step.Outcome == OutcomeProceed || step.Outcome == OutcomeStop {
			return step, n + 1, nil
		}
	}
	return last, p.Checks + 1, nil
}

// Missing names the unreadable FAE-HOST-001 sample fields, for gap records.
func Missing(s Sample) []string {
	var out []string
	for _, f := range []struct {
		name string
		r    Reading
	}{{"memory_available", s.Memory}, {"swap_total", s.Swap}, {"io_full_avg10", s.IO10}, {"io_full_avg60", s.IO60}, {"memory_full_avg10", s.Memory10}, {"memory_full_avg60", s.Memory60}, {"load_per_core", s.LoadPerCore}} {
		if !known(f.r) {
			out = append(out, f.name)
		}
	}
	return out
}

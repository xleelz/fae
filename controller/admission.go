package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

const (
	// LandingFloor is the Appendix A landing memory floor (FAE-HOST-007).
	LandingFloor = 12 * hostguard.GiB
	// ControllerClass is the heavy-op priority class of operator and
	// controller commands (FAE-SCH-011: role 0, controller 1, operator 2).
	ControllerClass = 1
)

// HeavyClasses maps the heavy-op priority class names to their ranks.
var HeavyClasses = map[string]int{"role": 0, "controller": ControllerClass, "operator": 2}

// AdmissionTimeout reports an expired bounded wait: infrastructure, not failure.
type AdmissionTimeout struct{ Stage, Detail string }

func (e *AdmissionTimeout) Error() string {
	return "heavy admission wait expired at " + e.Stage + ": " + e.Detail
}

// AdmissionRefused reports an immediate refusal without back-off.
type AdmissionRefused struct{ Reason string }

func (e *AdmissionRefused) Error() string { return "heavy admission refused: " + e.Reason }

// HeavyAdmission is the host admission of one host-heavy command run outside
// the daemon's drive: `fae gate run`, landing, the controller roll's package
// build and the `heavy-op run` broker (FAE-HOST-001..003, -007, -010, -031).
// Every reading applies the hard breaker (an immediate refusal), the soft
// pressure hold (FAE-HOST-002, -005), the landing memory floor when one is set
// and the two-reading I/O gate; only then is the shared heavy-op lease
// requested, and one fresh reading under the granted lease must still be clear
// of pressure. Every lease access reconciles proven-dead owners. Every wait is
// bounded and loud: each slice emits one alert line naming what it waits for,
// including the current lease holders.
//
// Every line that records a host reading (a wait slice, a timeout, a hard
// refusal, and the reading that lifts a pressure hold or the I/O gate after
// a recorded wait) carries memory available, swap, load per core and the PSI
// full avg10 and avg60 of I/O and memory, each with its direction since the
// previous reading (FAE-HOST-005), as the daemon's admission log does. The
// reading that starts a hold (soft pressure, the landing floor, or I/O
// pressure at the gate) is recorded at once, however short the wait, so the
// reading that lifts it is recorded too; the readings in between are
// recorded once per slice. The routine two-reading I/O window, whose
// readings are clear, and the lease queue stay quiet for a slice.
type HeavyAdmission struct {
	Ledger  *scheduler.Ledger
	Owner   scheduler.Owner
	Observe func(context.Context) (hostguard.Sample, error)
	Clock   hostguard.Clock
	Policy  hostguard.Policy
	Config  hostguard.Config
	// MemoryFloor is the landing floor of heavy controller operations
	// (default LandingFloor); a negative floor applies only the soft floor.
	MemoryFloor int64
	Weight      int
	// Class names the priority class: role, controller (default) or operator.
	Class string
	// MaxWait bounds the whole admission. HostChecks, when positive, bounds
	// the host readings to the first plus that many re-checks, so a role
	// command backs off at most Policy.Checks times (FAE-HOST-002) and then
	// refuses; zero waits for the host until MaxWait.
	MaxWait, Slice time.Duration
	HostChecks     int
	Alert          func(string)
	// Probe reports a foreign ledger owner's liveness; nil selects
	// scheduler.ProbeOwner.
	Probe func(scheduler.Owner) scheduler.Liveness
	// Hard is told of a hard-breaker refusal so its caller can raise the
	// FAE-HOST-003 alert; the refusal itself is also an Alert line.
	Hard func(hostguard.Sample)
}

func (a HeavyAdmission) defaults() HeavyAdmission {
	if a.Clock == nil {
		a.Clock = hostguard.SystemClock{}
	}
	if a.Policy == (hostguard.Policy{}) {
		a.Policy = hostguard.DefaultPolicy()
	}
	if a.Config == (hostguard.Config{}) {
		a.Config = hostguard.DefaultConfig()
	}
	if a.MemoryFloor == 0 {
		a.MemoryFloor = LandingFloor
	}
	if a.Weight == 0 {
		a.Weight = 1
	}
	if a.Class == "" {
		a.Class = "controller"
	}
	if a.MaxWait == 0 {
		a.MaxWait = 30 * time.Minute
	}
	if a.Slice == 0 {
		a.Slice = time.Minute
	}
	if a.Alert == nil {
		a.Alert = func(string) {}
	}
	return a
}

// Acquire returns the lease release; it never runs anything itself.
func (a HeavyAdmission) Acquire(ctx context.Context, id string) (func() error, error) {
	a = a.defaults()
	class, known := HeavyClasses[a.Class]
	if a.Ledger == nil || a.Observe == nil || id == "" || !known || a.Weight < 1 || a.MaxWait < 0 || a.HostChecks < 0 || a.Slice <= 0 || !hostguard.ValidPolicy(a.Policy) || a.MemoryFloor >= 0 && a.MemoryFloor < a.Config.Hard {
		return nil, &AdmissionRefused{"invalid heavy admission configuration"}
	}
	w := &admissionWait{HeavyAdmission: a, id: id, class: class, owners: scheduler.NewOwnerCache(a.Owner, a.Probe)}
	w.announced = a.Clock.Now()
	w.deadline = w.announced.Add(a.MaxWait)
	var e error
	if w.gate, e = hostguard.NewIOGate(a.Policy); e == nil {
		w.hold, e = hostguard.NewPressureHold(a.Policy)
	}
	if e != nil {
		return nil, &AdmissionRefused{e.Error()}
	}
	for {
		if e := w.host(ctx); e != nil {
			return nil, e
		}
		release, e := w.lease(ctx)
		if e != nil {
			return nil, e
		}
		clear, e := w.underLease(ctx)
		if e == nil && clear {
			return release, nil
		}
		if releaseErr := release(); releaseErr != nil || e != nil {
			return nil, errors.Join(e, releaseErr)
		}
		if !w.Clock.Now().Before(w.deadline) {
			w.Alert(fmt.Sprintf("gate timeout stage=pressure id=%s pressure under the granted lease", w.id))
			return nil, &AdmissionTimeout{"pressure", "pressure under the granted lease"}
		}
		if e := w.Clock.Sleep(ctx, w.Policy.Interval); e != nil {
			return nil, e
		}
	}
}

type admissionWait struct {
	HeavyAdmission
	id                  string
	class               int
	owners              *scheduler.OwnerCache
	gate, hold          *hostguard.Hold
	announced, deadline time.Time
	checks              int
	// recorded names the stages whose wait emitted a line; the pressure and
	// io entries clear when their hold lifts, so the lifting reading of a
	// recorded wait is recorded too.
	recorded map[string]bool
}

// loud emits one line per completed wait slice; a wait shorter than one slice
// (such as the routine two-reading I/O window) stays quiet. A held reading
// (one that starts or keeps a hold, not a clear reading of the routine I/O
// window or a lease poll) is recorded at once when its stage has no line yet,
// so the first reading of every hold is recorded, and through lifted the
// reading that lifts it (FAE-HOST-005).
func (w *admissionWait) loud(stage, detail string, held bool) {
	now := w.Clock.Now()
	if now.Sub(w.announced) >= w.Slice || held && !w.recorded[stage] {
		w.announced = now
		if w.recorded == nil {
			w.recorded = map[string]bool{}
		}
		w.recorded[stage] = true
		w.Alert(fmt.Sprintf("gate waiting stage=%s id=%s %s", stage, w.id, detail))
	}
}

// lifted records the reading that lifts a hold whose wait was recorded.
func (w *admissionWait) lifted(stage string, step hostguard.Step, o hostguard.Observation) {
	if step.Lifted && w.recorded[stage] {
		delete(w.recorded, stage)
		w.Alert(fmt.Sprintf("gate lifted stage=%s id=%s %s: %s %s", stage, w.id, step.Outcome, step.Reason, admissionReading(o)))
	}
}

// hard refuses on the FAE-HOST-003 hard breaker at once, without back-off.
func (w *admissionWait) hard(o hostguard.Observation) error {
	w.Alert(fmt.Sprintf("gate refused stage=host id=%s reason=HostHardLimit %s", w.id, admissionReading(o)))
	if w.Hard != nil {
		w.Hard(o.Sample)
	}
	return &AdmissionRefused{"HostHardLimit"}
}

// admissionReading formats one reading as the daemon's admission log records
// it (FAE-HOST-005): memory available and swap total in bytes, load per core,
// then the PSI full avg10 and avg60 of I/O and of memory, each with its
// direction since the pressure hold's previous reading.
func admissionReading(o hostguard.Observation) string {
	return fmt.Sprintf("memory_available=%s swap_total=%s load_per_core=%s io_full_avg10=%s io_full_avg60=%s io_avg10_dir=%s io_avg60_dir=%s memory_full_avg10=%s memory_full_avg60=%s memory_avg10_dir=%s memory_avg60_dir=%s",
		formatBytes(o.Sample.Memory), formatBytes(o.Sample.Swap), reading(o.Sample.LoadPerCore),
		reading(o.IO.Avg10), reading(o.IO.Avg60), o.IO.Dir10, o.IO.Dir60,
		reading(o.Memory.Avg10), reading(o.Memory.Avg60), o.Memory.Dir10, o.Memory.Dir60)
}

// host waits, bounded, until one reading passes the soft pressure hold, the
// landing floor and the two-reading I/O gate.
func (w *admissionWait) host(ctx context.Context) error {
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		sample, observeErr := w.Observe(ctx)
		at := w.Clock.Now()
		// The pressure hold records every reading first, so each line below,
		// the hard refusal included, carries the reading's directions.
		soft := w.hold.Record(at, sample, w.Config)
		if hostguard.Admit(sample, w.Config, 0, true).Hard {
			return w.hard(soft.Reading)
		}
		w.lifted("pressure", soft, soft.Reading)
		memory := GateResult{Passed: true}
		if w.MemoryFloor > 0 {
			memory = MemoryGate(sample, w.MemoryFloor)
		}
		io, ioStep := IOGate(w.gate, at, sample, w.Config)
		w.lifted("io", ioStep, soft.Reading)
		stage, detail, held := "", "", true
		switch {
		case !memory.Passed:
			stage, detail = "memory", memory.Reason
			if observeErr != nil {
				detail += " (" + observeErr.Error() + ")"
			}
		case soft.Outcome != hostguard.OutcomeProceed:
			// A soft outcome other than proceed always means a hold in force.
			stage, detail = "pressure", fmt.Sprintf("%s: %s", soft.Outcome, soft.Reason)
		case !io.Passed:
			// Only I/O pressure at the gate holds; a clear reading still
			// waiting for its second is the routine window.
			stage, detail, held = "io", io.Reason, ioStep.Outcome == hostguard.OutcomeRefused
		default:
			return nil
		}
		detail += " " + admissionReading(soft.Reading)
		if !at.Before(w.deadline) || w.HostChecks > 0 && w.checks >= w.HostChecks {
			w.Alert(fmt.Sprintf("gate timeout stage=%s id=%s %s", stage, w.id, detail))
			return &AdmissionTimeout{stage, detail}
		}
		w.checks++
		w.loud(stage, detail, held)
		if e := w.Clock.Sleep(ctx, w.Policy.Interval); e != nil {
			return e
		}
	}
}

// lease queues for the shared heavy-op lease and waits FIFO for it. Every
// access advances the ledger with the observed liveness of its owners, so an
// entry of a proven-dead owner is reclaimed (FAE-HOST-031).
func (w *admissionWait) lease(ctx context.Context) (func() error, error) {
	request := scheduler.Request{ID: w.id, Owner: w.Owner, Weight: w.Weight, Class: w.class, Queued: time.Now().UTC()}
	if e := w.Ledger.Enqueue(ctx, request); e != nil {
		return nil, &AdmissionRefused{e.Error()}
	}
	cancel := func(reason string) {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = w.Ledger.Cancel(cleanup, w.id, w.Owner, reason)
	}
	pool, err := w.Ledger.Snapshot(ctx)
	for {
		var events []scheduler.Event
		if err == nil {
			_, events, err = w.Ledger.Advance(ctx, time.Now().UTC(), w.owners.Observe(pool))
		}
		if err == nil {
			pool, err = w.Ledger.Snapshot(ctx)
		}
		if err != nil {
			cancel("ledger access failed")
			return nil, &AdmissionRefused{err.Error()}
		}
		for _, event := range events {
			if event.Kind == "Waiting" && event.RequestID == w.id {
				w.Alert(fmt.Sprintf("host lease waiting reason=HeavyLease id=%s wait=%s holders=%s", w.id, event.Wait, orNone(strings.Join(event.Holders, ","))))
			}
		}
		holders, queued := []string{}, false
		for _, h := range pool.Holders {
			if h.ID == w.id && h.Owner == w.Owner {
				return func() error {
					cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
					defer done()
					return w.Ledger.Release(cleanup, w.id, w.Owner, "heavy command completed")
				}, nil
			}
			holders = append(holders, fmt.Sprintf("%s(pid=%d,weight=%d,class=%d)", h.ID, h.Owner.PID, h.Weight, h.Class))
		}
		for _, r := range pool.Waiting {
			queued = queued || r.ID == w.id
		}
		detail := "holders=" + orNone(strings.Join(holders, ","))
		if !queued {
			w.Alert(fmt.Sprintf("gate timeout stage=lease id=%s %s", w.id, detail))
			return nil, &AdmissionTimeout{"lease", "heavy-op queue wait expired; " + detail}
		}
		if !w.Clock.Now().Before(w.deadline) {
			cancel("heavy admission wait expired")
			w.Alert(fmt.Sprintf("gate timeout stage=lease id=%s %s", w.id, detail))
			return nil, &AdmissionTimeout{"lease", detail}
		}
		w.loud("lease", detail, false)
		if e := w.Clock.Sleep(ctx, w.Policy.Interval); e != nil {
			cancel("heavy admission cancelled")
			return nil, e
		}
	}
}

// underLease is the FAE-HOST-001 reading under the granted lease. The lease is
// never held through a back-off: pressure returns to the host wait (whose
// pressure hold now needs two clearing readings), the hard breaker refuses.
// A reading that sends the admission back is part of the wait's record.
func (w *admissionWait) underLease(ctx context.Context) (bool, error) {
	sample, _ := w.Observe(ctx)
	soft := w.hold.Record(w.Clock.Now(), sample, w.Config)
	if hostguard.Admit(sample, w.Config, 0, true).Hard {
		return false, w.hard(soft.Reading)
	}
	w.lifted("pressure", soft, soft.Reading)
	if w.MemoryFloor > 0 {
		if memory := MemoryGate(sample, w.MemoryFloor); !memory.Passed {
			w.loud("memory", memory.Reason+" under the granted lease "+admissionReading(soft.Reading), true)
			return false, nil
		}
	}
	if soft.Outcome != hostguard.OutcomeProceed {
		w.loud("pressure", fmt.Sprintf("%s: %s under the granted lease %s", soft.Outcome, soft.Reason, admissionReading(soft.Reading)), true)
		return false, nil
	}
	return true, nil
}

func reading(r hostguard.Reading) string {
	if !r.Known {
		return "unknown"
	}
	return strconv.FormatFloat(r.Value, 'f', 2, 64)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

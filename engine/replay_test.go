package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// FAE-ROB-011: a planned graceful restart replays without any budget change;
// a crash still charges crash recovery and parks the turn.
func TestPlannedReplayChargesNothingAndCrashCharges(t *testing.T) {
	j, e := engine.NewLive("replay", 1, defaults.Workflow(), "tree")
	if e != nil {
		t.Fatal(e)
	}
	executor := engine.NewScripted(defaults.HappyScript())
	for j.State != "run-red" {
		var d *engine.Dispatch
		if j, d, _, e = engine.Prepare(j); e != nil {
			t.Fatal(e)
		}
		if d != nil {
			r, _ := executor.Execute(context.Background(), *d)
			r.ExecutionKind = "live"
			if d.Config.ChangeProducer {
				r.NewTree = "tree-" + d.Stage
			}
			if j, _, e = engine.Complete(j, d.ID, r); e != nil {
				t.Fatal(e)
			}
		}
	}
	j, d, _, e := engine.Prepare(j)
	if e != nil || d == nil {
		t.Fatal(e)
	}
	before := roundTrip(t, j)
	replayed, events, e := engine.ReplayInterrupted(before, d.ID)
	if e != nil || replayed.InFlight != nil || replayed.Parked || len(events) != 1 || events[0].Attempt != d.Attempt {
		t.Fatal(e, events)
	}
	again, next, _, e := engine.Prepare(replayed)
	if e != nil || next == nil || next.ID == d.ID || again.Attempts != before.Attempts || again.TestRuns != before.TestRuns || again.Counts[d.Stage] != before.Counts[d.Stage] || again.Failures["dispatch-recovery"] != 0 || again.Failures["crash-recovery"] != 0 {
		t.Fatal("planned replay changed a budget", again.Attempts, before.Attempts, again.TestRuns, before.TestRuns, again.Failures)
	}
	crashed, replay, e := engine.Recover(before, false, false)
	if e != nil || replay != nil || !crashed.Parked || crashed.Reason != "UncertainDispatch" || crashed.Failures["crash-recovery"] != 1 {
		t.Fatal("crash did not charge recovery", e)
	}
	// A parked graceful turn listed by the marker still replays without charge.
	parked, _, _ := engine.Recover(before, true, false)
	if parked.Failures["crash-recovery"] != 0 {
		t.Fatal("graceful park charged")
	}
	if k, _, e := engine.ReplayInterrupted(parked, d.ID); e != nil || k.InFlight != nil {
		t.Fatal(e)
	}
	if _, _, e = engine.ReplayInterrupted(before, "other"); e == nil {
		t.Fatal("replayed a foreign dispatch")
	}
	held, _, e := engine.ApplyControl(before, engine.Control{Method: "Escalate", Reason: "hold"}, before.InFlight.CreatedAt)
	if e != nil || held.State != domain.Escalated {
		t.Fatal(e)
	}
	if _, _, e = engine.ReplayInterrupted(held, d.ID); e == nil {
		t.Fatal("replayed an operator hold")
	}
}

// FAE-ROB-018 census: every record of a run that defers, is operated on,
// crashes and recovers stages an explicit attempt and a cause.
func TestEveryRecordStagesAttemptAndCause(t *testing.T) {
	j := newJob(t)
	j, _, e := engine.ApplyControl(j, engine.Control{Method: "Defer", Until: time.Now().Add(time.Millisecond)}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	time.Sleep(5 * time.Millisecond)
	j, _ = firstDispatch(t, j)
	j, _, e = engine.Recover(j, false, false)
	if e != nil || !j.Parked {
		t.Fatal(e)
	}
	j, _, e = engine.ResolveUncertain(j, j.InFlight.ID, "escalate", nil)
	if e != nil {
		t.Fatal(e)
	}
	j, _, e = engine.ApplyControl(j, engine.Control{Method: "RestartWithInformation", Information: "retry"}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	j = drive(t, j, engine.NewScripted(padded(3)), true)
	if j.State != domain.Done {
		t.Fatal(j.State, j.Reason)
	}
	kinds := map[string]bool{}
	for _, event := range j.History {
		kinds[event.Kind] = true
		if event.Attempt < 1 || event.Cause == "" {
			t.Fatalf("record lacks staged context: %+v", event)
		}
	}
	for _, kind := range []string{"undefer", "crash-recovery", "uncertainty-resolved", "task-control", "transition", "dispatch", "gate"} {
		if !kinds[kind] {
			t.Fatal("census missed", kind)
		}
	}
}

// FAE-ROB-012/-016: an interrupted local effect whose completed receipt
// survives is adopted at recovery without re-running it or charging any
// budget; an agent turn or an operator hold is never adopted this way.
func TestAdoptInterruptedEffectChargesNothing(t *testing.T) {
	j, e := engine.NewLive("adopt", 1, defaults.Workflow(), "tree")
	if e != nil {
		t.Fatal(e)
	}
	executor := engine.NewScripted(defaults.HappyScript())
	var d *engine.Dispatch
	for {
		if j, d, _, e = engine.Prepare(j); e != nil {
			t.Fatal(e)
		}
		if d == nil {
			continue
		}
		if d.Stage == "format" {
			break
		}
		r, _ := executor.Execute(context.Background(), *d)
		r.ExecutionKind = "live"
		if d.Config.ChangeProducer {
			r.NewTree = "tree-" + d.Stage
		}
		if j, _, e = engine.Complete(j, d.ID, r); e != nil {
			t.Fatal(e)
		}
	}
	before := roundTrip(t, j)
	r, _ := executor.Execute(context.Background(), *d)
	r.ExecutionKind = "live"
	adopted, events, e := engine.AdoptInterruptedEffect(before, d.ID, r)
	if e != nil || adopted.InFlight != nil || adopted.Parked || len(adopted.Provenance) != len(before.Provenance)+1 || len(adopted.ResolvedDispatches) != 1 {
		t.Fatal("completed receipt not adopted", e)
	}
	if adopted.Attempts != before.Attempts || adopted.Failures["crash-recovery"] != 0 || adopted.Failures["dispatch-recovery"] != 0 || events[len(events)-1].Kind != "effect-receipt-adopted" {
		t.Fatal("adoption charged a budget or left no record", adopted.Failures, events)
	}
	// A parked uncertain effect adopts the same way; its earlier crash charge stays.
	parked, _, _ := engine.Recover(before, false, false)
	if again, _, e := engine.AdoptInterruptedEffect(parked, d.ID, r); e != nil || again.InFlight != nil || again.Failures["crash-recovery"] != 1 {
		t.Fatal("parked effect not adopted", e)
	}
	if _, _, e = engine.AdoptInterruptedEffect(before, "other", r); e == nil {
		t.Fatal("adopted a foreign dispatch")
	}
	bad := r
	bad.Class = "uncertain"
	if _, _, e = engine.AdoptInterruptedEffect(before, d.ID, bad); e == nil {
		t.Fatal("adopted a classified result")
	}
	held, _, e := engine.ApplyControl(before, engine.Control{Method: "Escalate", Reason: "hold"}, before.InFlight.CreatedAt)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = engine.AdoptInterruptedEffect(held, d.ID, r); e == nil {
		t.Fatal("adopted under an operator hold")
	}
	turn, _ := firstDispatch(t, newJob(t))
	if _, _, e = engine.AdoptInterruptedEffect(turn, turn.InFlight.ID, r); e == nil {
		t.Fatal("adopted an agent turn")
	}
}

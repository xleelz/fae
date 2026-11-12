package engine_test

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/workflow"
	"testing"
)

func uncertainJob(t *testing.T) engine.Job {
	t.Helper()
	j, err := engine.NewLive("uncertain", 1, defaults.Workflow(), "base")
	if err != nil {
		t.Fatal(err)
	}
	for j.InFlight == nil {
		j, _, _, err = engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
	}
	j, _, err = engine.Recover(j, false, false)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func TestUncertainResolutionBudgetIdentityAndEvidence(t *testing.T) {
	j := uncertainJob(t)
	old := j.InFlight.ID
	for i := 0; i < 3; i++ {
		var err error
		j, _, err = engine.Recover(j, false, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	if j.Failures["crash-recovery"] != 1 {
		t.Fatal("restarts double charged", j.Failures)
	}
	if _, _, err := engine.ResolveUncertain(j, "stale", "redispatch", nil); err == nil {
		t.Fatal("accepted stale identity")
	}
	k, _, err := engine.ResolveUncertain(j, old, "redispatch", nil)
	if err != nil {
		t.Fatal(err)
	}
	if k.Attempts != j.Attempts || k.Counts[j.InFlight.Stage] != j.Counts[j.InFlight.Stage] || len(k.ResolvedDispatches) != 1 {
		t.Fatal("budgets or evidence lost")
	}
	k, d, _, err := engine.Prepare(k)
	if err != nil || d == nil || d.ID == old || d.Sessions[0].ID == j.InFlight.Sessions[0].ID {
		t.Fatal("not a fresh replacement", err)
	}
	k, _, err = engine.Recover(k, false, false)
	if err != nil {
		t.Fatal(err)
	}
	k.Failures["crash-recovery"] = 3
	if _, _, err = engine.ResolveUncertain(k, d.ID, "redispatch", nil); err == nil {
		t.Fatal("unbounded crash retry")
	}
	k, _, err = engine.ResolveUncertain(k, d.ID, "escalate", nil)
	if err != nil || k.InFlight != nil || k.EscalationKind != domain.DispatchRecoveryExhausted || k.Reason != "DispatchRecoveryExhausted: UncertainDispatch "+d.ID {
		t.Fatal(k, err)
	}
	b, _ := json.Marshal(k)
	var persisted engine.Job
	if err = json.Unmarshal(b, &persisted); err != nil || len(persisted.ResolvedDispatches) != 2 {
		t.Fatal("lost immutable recovery evidence", err)
	}
}
func TestUncertainAcceptStrictRecordedOutput(t *testing.T) {
	j := uncertainJob(t)
	d := j.InFlight
	r, err := engine.NewScripted(defaults.HappyScript()).Execute(context.Background(), *d)
	if err != nil {
		t.Fatal(err)
	}
	r.ExecutionKind = "live"
	for _, mutate := range []func(*engine.Result){func(r *engine.Result) { r.Session = "forged" }, func(r *engine.Result) { r.Tree = "foreign" }, func(r *engine.Result) { r.Output = json.RawMessage(`{}`) }, func(r *engine.Result) { r.NewTree = "changed" }, func(r *engine.Result) { r.Class = "uncertain" }} {
		bad := r
		mutate(&bad)
		if _, _, err = engine.ResolveUncertain(j, d.ID, "accept", &bad); err == nil {
			t.Fatal("accepted invalid recorded output", bad)
		}
	}
	k, _, err := engine.ResolveUncertain(j, d.ID, "accept", &r)
	if err != nil || len(k.Provenance) != 1 || len(k.ResolvedDispatches) != 1 {
		t.Fatal(k, err)
	}
	if _, _, err = engine.ResolveUncertain(k, d.ID, "accept", &r); err == nil {
		t.Fatal("duplicate accept")
	}
}
func TestUncertaintyGuardTruthTable(t *testing.T) {
	for bits := 0; bits < 32; bits++ {
		if got := kernel.CanResolveUncertain(bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0, bits&16 != 0); got != (bits == 31) {
			t.Fatal(bits)
		}
	}
}

// FAE-ROB-013: a crash-interrupted engine run is infrastructure, not a stage
// failure: it re-runs on the unchanged tree with its charges refunded and no
// failure recorded, consuming only the dispatch-recovery budget (FAE-ROB-011,
// -017), which escalates DispatchRecoveryExhausted past its bound.
func TestInterruptedEngineRunRerunsWithoutFailureVerdict(t *testing.T) {
	j, err := engine.NewLive("rerun", 1, defaults.Workflow(), "base")
	if err != nil {
		t.Fatal(err)
	}
	executor := engine.NewScripted(defaults.HappyScript())
	var d *engine.Dispatch
	for i := 0; i < 100; i++ {
		if j, d, _, err = engine.Prepare(j); err != nil {
			t.Fatal(err)
		}
		if d == nil {
			continue
		}
		if d.Config.Primitive == workflow.HostCommand {
			break
		}
		r, err := executor.Execute(context.Background(), *d)
		if err != nil {
			t.Fatal(err)
		}
		r.ExecutionKind = "live"
		if j, _, err = engine.Complete(j, d.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	if d == nil || d.Config.Primitive != workflow.HostCommand {
		t.Fatal("no engine run reached")
	}
	stage, attempts, runs, visits := d.Stage, j.Attempts, j.TestRuns, j.Counts[d.Stage]
	charged := func(d *engine.Dispatch) int {
		if d.AttemptCharged {
			return 1
		}
		return 0
	}
	for n := 1; n <= 3; n++ {
		// At startup the crashed run is still in flight, before any park.
		rerun, events, err := engine.RerunInterrupted(j, j.InFlight.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Kind == "failure" {
				t.Fatal("interrupted engine run recorded a failure verdict")
			}
		}
		if n == 3 {
			if rerun.EscalationKind != domain.DispatchRecoveryExhausted {
				t.Fatal("dispatch recoveries unbounded", rerun.State, rerun.Reason)
			}
			break
		}
		if rerun.InFlight != nil || rerun.Parked || rerun.Failures["dispatch-recovery"] != n || len(rerun.ResolvedDispatches) != n || rerun.Attempts != attempts-charged(d) || rerun.TestRuns != runs-1 || rerun.Counts[stage] != visits-1 || events[0].Kind != "crash-rerun" {
			t.Fatal("rerun charged or lost the interrupted run", rerun.Failures, rerun.Attempts, rerun.TestRuns, events)
		}
		if j, d, _, err = engine.Prepare(rerun); err != nil || d == nil || d.Stage != stage || d.ID == rerun.ResolvedDispatches[n-1].ID {
			t.Fatal("interrupted run not re-dispatched fresh", err)
		}
	}
	held := uncertainJob(t)
	if _, _, err = engine.RerunInterrupted(held, held.InFlight.ID); err == nil {
		t.Fatal("an interrupted agent turn re-ran without resolution")
	}
}

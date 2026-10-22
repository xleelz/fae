package engine_test

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/kernel"
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
	if err != nil || k.InFlight != nil || k.Reason != "UncertainDispatchEscalated" {
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

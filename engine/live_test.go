package engine_test

import (
	"context"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
)

func TestLiveTreeEvidenceAndKindBoundary(t *testing.T) {
	j, err := engine.NewLive("live", 1, defaults.Workflow(), "base")
	if err != nil {
		t.Fatal(err)
	}
	script := engine.NewScripted(defaults.HappyScript())
	changed := 0
	for i := 0; i < 150 && j.State != domain.Done; i++ {
		next, d, _, err := engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		j = next
		if j.Parked || j.State == domain.Escalated {
			t.Fatalf("unexpected stop %s %s", j.State, j.Reason)
		}
		if d == nil {
			continue
		}
		r, err := script.Execute(context.Background(), *d)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := engine.Complete(j, d.ID, r); err == nil {
			t.Fatal("scripted output accepted as live")
		}
		r.ExecutionKind = "live"
		if d.Config.ChangeProducer {
			changed++
			if changed == 1 {
				r.NewTree = "red-tree"
			} else {
				r.NewTree = "green-tree"
			}
		}
		if d.Config.Primitive == workflow.Review {
			bad := r
			bad.NewTree = "unreviewed"
			if _, _, err := engine.Complete(j, d.ID, bad); err == nil {
				t.Fatal("review mutated tree")
			}
		}
		j, _, err = engine.Complete(j, d.ID, r)
		if err != nil {
			t.Fatal(err)
		}
		if err = engine.Validate(roundTrip(t, j)); err != nil {
			t.Fatal(err)
		}
	}
	if j.State != domain.Done || j.Tree != "green-tree" || changed != 2 {
		t.Fatalf("%s tree %s changes %d", j.State, j.Tree, changed)
	}
	for _, p := range j.Provenance {
		if p.ExecutionKind != "live" {
			t.Fatal("wrong kind")
		}
	}
	j.Provenance[0].ExecutionKind = "scripted"
	if engine.Validate(j) == nil {
		t.Fatal("relabeled artifact accepted")
	}
}

func TestLiveRecoveryNeverReplays(t *testing.T) {
	j, err := engine.NewLive("live", 1, defaults.Workflow(), "base")
	if err != nil {
		t.Fatal(err)
	}
	j, _, _, err = engine.Prepare(j)
	if err != nil {
		t.Fatal(err)
	}
	j, d, _, err := engine.Prepare(j)
	if err != nil || d == nil {
		t.Fatal(err)
	}
	for _, graceful := range []bool{false, true} {
		recovered, replay, err := engine.Recover(j, graceful, true)
		if err != nil || replay != nil || !recovered.Parked || recovered.Reason != "UncertainDispatch" || recovered.InFlight.ID != d.ID {
			t.Fatalf("unsafe live recovery %+v %v", recovered, err)
		}
	}
}

func TestReconciliationRequiresNewTreeTestsAndReviews(t *testing.T) {
	doc := defaults.Workflow()
	j, err := engine.NewLive("reconcile", 1, doc, "original")
	if err != nil {
		t.Fatal(err)
	}
	j.Base = "old-base"
	scripts := defaults.HappyScript()
	for stage, steps := range scripts {
		if len(steps) == 1 {
			scripts[stage] = append(steps, steps[0], steps[0])
		}
	}
	executor := engine.NewScripted(scripts)
	reconciled := false
	for n := 0; n < 200 && j.State != domain.Done; n++ {
		next, d, _, err := engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		j = next
		if j.State == domain.Escalated || j.Parked {
			t.Fatalf("stopped %s: %s", j.State, j.Reason)
		}
		if d == nil {
			continue
		}
		r, err := executor.Execute(context.Background(), *d)
		if err != nil {
			t.Fatal(err)
		}
		r.ExecutionKind = "live"
		if d.Config.Primitive == workflow.Merge && !reconciled {
			r.NewTree = "merged"
			r.NewBase = "new-base"
			reconciled = true
		}
		j, _, err = engine.Complete(j, d.ID, r)
		if err != nil {
			t.Fatal(err)
		}
		if r.NewTree != "" {
			if j.State != "reverify-loop" || j.Base != "new-base" {
				t.Fatal("reverification route missing")
			}
			forged := j
			forged.State = "commit"
			blocked, attempted, _, err := engine.Prepare(forged)
			if err != nil {
				t.Fatal(err)
			}
			if attempted != nil || (!blocked.Parked && blocked.State != domain.Escalated) {
				t.Fatal("old approvals authorize merged tree")
			}
		}
	}
	if j.State != domain.Done || j.Counts["run-tests"] != 2 || j.Counts["reviews"] != 2 || j.Counts["merge"] != 2 {
		t.Fatalf("not reverified: %s %+v", j.State, j.Counts)
	}
}

func TestPendingDispatchPinsDurableBase(t *testing.T) {
	j, err := engine.NewLive("pinned-base", 1, defaults.Workflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	j.Base = "original-base"
	var dispatch *engine.Dispatch
	for i := 0; i < 10 && dispatch == nil; i++ {
		j, dispatch, _, err = engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err != nil || dispatch == nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := engine.Validate(roundTrip(t, j)); err != nil {
		t.Fatal(err)
	}
	j.Base = "unrecorded-base"
	if engine.Validate(roundTrip(t, j)) == nil {
		t.Fatal("accepted base mutation with outstanding dispatch")
	}
}

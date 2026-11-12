package engine_test

import (
	"context"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"strings"
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

// FAE-VCS-005: a clean reconcile that changed the tree re-validates. When it
// is patch-identical only QA re-runs and the approvals carry (an evidence rule
// guarded by CanCarryReview); otherwise every gate re-runs on the new tree.
func TestReconciliationRequiresNewTreeTestsAndReviews(t *testing.T) {
	for _, identical := range []bool{true, false} {
		t.Run(map[bool]string{true: "patch-identical re-runs QA only", false: "changed patch re-verifies"}[identical], func(t *testing.T) {
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
			if !identical {
				scripts["merge"][0].Output = []byte(`{"result":"clean","patch_identical":false}`)
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
				if d.Stage == "commit" && identical {
					// Without the recorded carry the old approvals never authorize.
					forged := j
					forged.InFlight = nil
					forged.Sequence--
					forged.Counts = map[string]int{}
					for k, v := range j.Counts {
						forged.Counts[k] = v
					}
					forged.Counts["commit"]--
					forged.History = forged.History[:len(forged.History)-1]
					forged.Attempts = d.Attempt
					if _, again, _, err := engine.Prepare(forged); err != nil || again == nil {
						t.Fatal("rewound commit dispatch differs", err)
					}
					forged.Carries = nil
					if blocked, attempted, _, err := engine.Prepare(forged); err != nil || attempted != nil || blocked.State != domain.Escalated || blocked.EscalationKind != domain.ResumeEvidence || !strings.Contains(blocked.EscalationDetail, "review") {
						t.Fatalf("approvals carried without a recorded carry: %v %s %s", err, blocked.State, blocked.EscalationDetail)
					}
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
					if want := map[bool]string{true: "merge-route", false: "reverify-loop"}[identical]; j.State != domain.State(want) || j.Base != "new-base" {
						t.Fatal("reconcile route", j.State, "want", want)
					}
					forged := j
					forged.State = "commit"
					blocked, attempted, _, err := engine.Prepare(forged)
					if err != nil {
						t.Fatal(err)
					}
					if attempted != nil || (!blocked.Parked && blocked.State != domain.Escalated) {
						t.Fatal("old test evidence authorizes merged tree")
					}
				}
			}
			if j.State != domain.Done {
				t.Fatal(j.State, j.Reason)
			}
			if identical && (j.Counts["run-tests"] != 1 || j.Counts["reviews"] != 1 || j.Counts["oracle"] != 1 || j.Counts["qa"] != 1 || j.Counts["post-merge-qa"] != 1 || len(j.Carries) != 1 || j.Carries[0].To != "merged") {
				t.Fatalf("patch-identical reconcile did not re-run QA only: %+v %+v", j.Counts, j.Carries)
			}
			if !identical && (j.Counts["run-tests"] != 2 || j.Counts["reviews"] != 2 || j.Counts["merge"] != 2 || len(j.Carries) != 0) {
				t.Fatalf("not reverified: %+v", j.Counts)
			}
		})
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

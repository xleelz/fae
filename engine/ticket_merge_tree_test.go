package engine_test

import (
	"context"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
)

func TestChangedTreeInvalidatesReviewMerge(t *testing.T) {
	for _, kind := range []string{"change-producer", "reconciliation"} {
		t.Run(kind, func(t *testing.T) {
			j, err := engine.NewLive("merge-proof", 1, defaults.Workflow(), "merged-tree")
			if err != nil {
				t.Fatal(err)
			}
			script := engine.NewScripted(defaults.HappyScript())
			for n := 0; n < 150; n++ {
				next, d, _, err := engine.Prepare(j)
				if err != nil {
					t.Fatal(err)
				}
				j = next
				if d == nil {
					continue
				}
				r, err := script.Execute(context.Background(), *d)
				if err != nil {
					t.Fatal(err)
				}
				r.ExecutionKind = "live"
				selected := (kind == "change-producer" && d.Config.ChangeProducer) || (kind == "reconciliation" && d.Config.Primitive == workflow.Merge)
				if selected {
					j.ReviewMergeRequired = true
					j.ReviewMerged = true
					j.ReviewID = "review-1"
					same := r
					same.NewTree = j.Tree
					unchanged, _, err := engine.Complete(j, d.ID, same)
					if err != nil || !unchanged.ReviewMerged {
						t.Fatal("same tree lost merge proof", err)
					}
					r.NewTree = "different-tree"
					changed, _, err := engine.Complete(j, d.ID, r)
					if err != nil || changed.ReviewMerged {
						t.Fatal("changed tree retained stale merge proof", err)
					}
					return
				}
				j, _, err = engine.Complete(j, d.ID, r)
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Fatal("selected stage not reached")
		})
	}
}

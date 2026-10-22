package engine_test

import (
	"context"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

func TestKnownIntegrationTargetMoveRefreshesAllApprovals(t *testing.T) {
	j, e := engine.NewLive("retry", 1, defaults.Workflow(), "reviewed")
	if e != nil {
		t.Fatal(e)
	}
	j.Base = "old-base"
	scripts := defaults.HappyScript()
	for key, v := range scripts {
		if len(v) == 1 {
			scripts[key] = append(scripts[key], v[0], v[0])
		}
	}
	executor := engine.NewScripted(scripts)
	moved := false
	reconciled := false
	for i := 0; i < 240 && j.State != domain.Done; i++ {
		var d *engine.Dispatch
		j, d, _, e = engine.Prepare(j)
		if e != nil {
			t.Fatal(e)
		}
		if j.Parked || j.State == domain.Escalated {
			t.Fatalf("stopped %s %s", j.State, j.Reason)
		}
		if d == nil {
			continue
		}
		r, e := executor.Execute(context.Background(), *d)
		if e != nil {
			t.Fatal(e)
		}
		r.ExecutionKind = "live"
		if d.Stage == "integrate" && !moved {
			r.Class = "target-moved"
			moved = true
		}
		if d.Stage == "merge" && moved && !reconciled {
			r.NewTree = "union"
			r.NewBase = "new-tip"
			reconciled = true
		}
		j, _, e = engine.Complete(j, d.ID, r)
		if e != nil {
			t.Fatal(e)
		}
		if r.Class == "target-moved" {
			if j.State != "integration-retry-loop" {
				t.Fatal("missing bounded retry")
			}
			forged := j
			forged.State = "commit"
			blocked, attempted, _, err := engine.Prepare(forged)
			if err != nil {
				t.Fatal(err)
			}
			if attempted != nil || (!blocked.Parked && blocked.State != domain.Escalated) {
				t.Fatal("stale approvals survived retry")
			}
		}
	}
	if j.State != domain.Done || j.Counts["commit"] != 2 || j.Counts["integrate"] != 2 || j.Counts["reviews"] != 2 || j.Counts["qa"] != 2 || j.Base != "new-tip" {
		t.Fatalf("no fully refreshed landing: %s %+v", j.State, j.Counts)
	}
}
func TestTargetMovedClassCannotForgeOtherTransitions(t *testing.T) {
	j, _ := engine.NewLive("wrong", 1, defaults.Workflow(), "tree")
	var d *engine.Dispatch
	for d == nil {
		j, d, _, _ = engine.Prepare(j)
	}
	r, _ := engine.NewScripted(defaults.HappyScript()).Execute(context.Background(), *d)
	r.ExecutionKind = "live"
	r.Class = "target-moved"
	if _, _, e := engine.Complete(j, d.ID, r); e == nil {
		t.Fatal("nonintegration retry admitted")
	}
}

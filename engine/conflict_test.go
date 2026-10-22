package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func TestConflictRoutesImplementationWithBoundedFreshApprovals(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		t.Run(fmt.Sprint(repeat), func(t *testing.T) {
			doc := defaults.Workflow()
			j, e := engine.NewLive("conflict", 1, doc, "old")
			if e != nil {
				t.Fatal(e)
			}
			j.Base = "base"
			scripts := defaults.HappyScript()
			for key, v := range scripts {
				if len(v) == 1 {
					for i := 0; i < 30; i++ {
						scripts[key] = append(scripts[key], v[0])
					}
				}
			}
			executor := engine.NewScripted(scripts)
			conflicts := 0
			namedInput := false
			for i := 0; i < 600 && !j.State.Terminal() && j.State != domain.Escalated; i++ {
				var d *engine.Dispatch
				j, d, _, e = engine.Prepare(j)
				if e != nil {
					t.Fatal(e)
				}
				if j.Parked {
					t.Fatal(j.Reason)
				}
				if d == nil {
					continue
				}
				r, e := executor.Execute(context.Background(), *d)
				if e != nil {
					t.Fatal(e)
				}
				r.ExecutionKind = "live"
				if d.Stage == "implement" && conflicts > 0 {
					for _, a := range d.Inputs {
						if a.Type == "ReconcileReport" && a.Fields["result"] == "conflicted" {
							namedInput = true
						}
					}
					r.NewTree = fmt.Sprintf("resolved-%d", conflicts)
				}
				if d.Config.Primitive == workflow.Merge && (conflicts == 0 || repeat) {
					conflicts++
					r.NewTree = fmt.Sprintf("markers-%d", conflicts)
					r.NewBase = fmt.Sprintf("tip-%d", conflicts)
					r.Output = json.RawMessage(`{"result":"conflicted","patch_identical":false,"paths":["x.go"],"original_base":"base","original_tree":"old","base":"tip","plan_key":"key"}`)
				}
				j, _, e = engine.Complete(j, d.ID, r)
				if e != nil {
					t.Fatal(e)
				}
				if d.Config.Primitive == workflow.Merge && r.NewTree != "" {
					if j.State != "merge-route" {
						t.Fatalf("conflict bypassed remediation: %s", j.State)
					}
					forged := j
					forged.State = "commit"
					blocked, attempted, _, e := engine.Prepare(forged)
					if e != nil {
						t.Fatal(e)
					}
					if attempted != nil || (!blocked.Parked && blocked.State != domain.Escalated) {
						t.Fatal("old approvals authorize markers")
					}
				}
			}
			if !namedInput {
				t.Fatal("implementation lacks named conflict artifact")
			}
			if repeat {
				if j.State != domain.Escalated || conflicts > 8 {
					t.Fatalf("unbounded conflicts %d %s", conflicts, j.State)
				}
			} else {
				if j.State != domain.Done || j.Counts["implement"] != 2 || j.Counts["reviews"] != 2 || j.Counts["qa"] != 2 {
					t.Fatalf("did not refresh complete approvals: %s %+v", j.State, j.Counts)
				}
			}
		})
	}
}

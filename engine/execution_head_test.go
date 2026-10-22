package engine_test

import (
	"context"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
	"time"
)

func TestExecutionHeadCommitProvenanceAndUncertainty(t *testing.T) {
	j, err := engine.NewLive("head", 1, defaults.Workflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	j.Base = "origin"
	if j.ExpectedHead() != "origin" {
		t.Fatal("legacy base fallback")
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
		if d.ExecutionHead != j.ExpectedHead() || d.Base != "origin" {
			t.Fatal("dispatch lost origin")
		}
		r, err := script.Execute(context.Background(), *d)
		if err != nil {
			t.Fatal(err)
		}
		r.ExecutionKind = "live"
		if d.Config.Primitive == workflow.VcsCommit {
			uncertain := r
			uncertain.Class = "uncertain"
			held, _, err := engine.Complete(j, d.ID, uncertain)
			if err != nil {
				t.Fatal(err)
			}
			if held.ExpectedHead() != "origin" {
				t.Fatal("uncertain commit advanced HEAD")
			}
			if _, _, err = engine.ApplyControl(held, engine.Control{Method: "RestartWithInformation", Information: "retry"}, time.Now()); err == nil {
				t.Fatal("uncertain commit restarted")
			}
		}
		j, _, err = engine.Complete(j, d.ID, r)
		if err != nil {
			t.Fatal(err)
		}
		if d.Config.Primitive == workflow.VcsCommit {
			if j.ExecutionHead == "" || j.ExecutionHead == j.Base {
				t.Fatal("commit did not pin HEAD")
			}
			if err = engine.Validate(roundTrip(t, j)); err != nil {
				t.Fatal(err)
			}
			bad := roundTrip(t, j)
			bad.ExecutionHead = "foreign"
			if engine.Validate(bad) == nil {
				t.Fatal("unproved head accepted")
			}
			j, _, err = engine.ApplyControl(j, engine.Control{Method: "Escalate", Reason: "new information"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			j, _, err = engine.ApplyControl(j, engine.Control{Method: "RestartWithInformation", Information: "retry"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if j.Base != "origin" || j.ExecutionHead == "" || j.State != domain.State(j.Document.Start) {
				t.Fatal("restart lost original base")
			}
			_, next, _, err := engine.Prepare(j)
			if err != nil {
				t.Fatal(err)
			}
			if next != nil && next.ExecutionHead != j.ExecutionHead {
				t.Fatal("restart used original head")
			}
			return
		}
	}
	t.Fatal("commit not reached")
}

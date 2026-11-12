package engine_test

import (
	"strings"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// Appendix B ResumeEvidence: a resumed task whose durable stage can no longer
// re-validate its required inputs escalates with the kind instead of parking
// forever; a task whose evidence holds, or that is in flight, is untouched.
func TestResumeRevalidatesDurableStageEvidence(t *testing.T) {
	j, err := engine.New("task", 1, defaults.Workflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	executor := engine.NewScripted(defaults.HappyScript())
	for string(j.State) != "test-first" || j.InFlight != nil {
		next, d, _, err := engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		if j = next; d == nil {
			continue
		}
		r, _ := executor.Execute(t.Context(), *d)
		if j, _, err = engine.Complete(j, d.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	same, events, err := engine.RevalidateResume(j)
	if err != nil || len(events) != 0 || same.State != j.State {
		t.Fatal("valid durable stage escalated", err, events)
	}
	// The plan evidence the stage stands on is gone (as after a lossy repair).
	lost := roundTrip(t, j)
	dispatched, _, _, err := engine.Prepare(lost)
	if err != nil || dispatched.InFlight == nil {
		t.Fatal("test-first did not dispatch", err)
	}
	if held, events, err := engine.RevalidateResume(dispatched); err != nil || len(events) != 0 || held.InFlight == nil {
		t.Fatal("in-flight task revalidated", err, events)
	}
	for i := len(lost.Evidence) - 1; i >= 0; i-- {
		if lost.Evidence[i].Stage == "plan" {
			lost.Evidence = append(lost.Evidence[:i], lost.Evidence[i+1:]...)
		}
	}
	got, events, err := engine.RevalidateResume(lost)
	if err != nil || got.State != domain.Escalated || got.EscalationKind != domain.ResumeEvidence || !strings.Contains(got.Reason, "[plan]") || len(events) == 0 {
		t.Fatal(got.State, got.Reason, err)
	}
	if _, _, err = engine.RevalidateResume(got); err != nil {
		t.Fatal("escalated task not idempotent", err)
	}
}

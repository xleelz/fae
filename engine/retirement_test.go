package engine_test

import (
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"testing"
	"time"
)

func TestRetirementStateMachine(t *testing.T) {
	for _, reason := range []string{"superseded-by next", "contained abc", "obsolete ruling/123"} {
		j := newJob(t)
		if _, _, err := engine.ApplyControl(j, engine.Control{Method: "Retire", Reason: reason}, time.Now()); err == nil {
			t.Fatal("Pending retired")
		}
		j.State = domain.Escalated
		got, ev, err := engine.ApplyControl(j, engine.Control{Method: "Retire", Reason: reason}, time.Now())
		if err != nil || got.State != domain.Retired || got.Reason != reason || len(ev) != 3 {
			t.Fatal(got, ev, err)
		}
		if _, _, err = engine.ApplyControl(got, engine.Control{Method: "RestartWithInformation", Information: "x"}, time.Now()); err == nil {
			t.Fatal("terminal restart")
		}
	}
	for _, reason := range []string{"", "obsolete", "obsolete ", "superseded-by a b", "contained a b", "other x"} {
		if _, _, err := engine.ParseRetirement(reason); err == nil {
			t.Fatal(reason)
		}
	}
	j, _ := firstDispatch(t, newJob(t))
	j = apply(t, j, engine.Control{Method: "Escalate", Reason: "hold"})
	if _, _, err := engine.ApplyControl(j, engine.Control{Method: "Retire", Reason: "obsolete ruling"}, time.Now()); err == nil {
		t.Fatal("in flight retired")
	}
}

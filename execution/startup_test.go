package execution

import (
	"context"
	"errors"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
	"testing"
)

type startupRuntime struct{ proof, retired bool }

func (s startupRuntime) Run(context.Context, runtime.Launch) (runtime.Collected, error) {
	return runtime.Collected{NonExecution: s.proof, ProcessesRetired: s.retired}, errors.New("startup failed")
}
func TestStartupRequiresPositiveProofAndRelease(t *testing.T) {
	for _, tt := range []struct {
		name                            string
		proof, retired, changed, health bool
	}{{"safe", true, true, false, false}, {"uncertain", false, true, false, false}, {"alive", true, false, false, false}, {"changed", true, true, true, false}, {"health-skipped", true, true, false, true}, {"executed-health-failed", false, true, false, true}} {
		t.Run(tt.name, func(t *testing.T) {
			x, d, a := fixture(t)
			x.Runtime = startupRuntime{tt.proof, tt.retired}
			calls := 0
			x.Tree = func(context.Context, Binding) (string, error) {
				calls++
				if tt.changed && calls > 1 {
					return "changed", nil
				}
				return "tree", nil
			}
			observed := 0
			if tt.health {
				x.Observe = func(context.Context, engine.Dispatch, engine.Session, providerhealth.Outcome) error {
					observed++
					return errors.New("health failed")
				}
			}
			r, e := x.Execute(context.Background(), d)
			want := "uncertain"
			if tt.name == "safe" || tt.name == "health-skipped" {
				want = "startup-retry"
			}
			if tt.health && observed != map[bool]int{true: 0, false: 1}[tt.proof] {
				t.Fatalf("incorrect provider health observation count: %d", observed)
			}
			if e == nil || r.Class != want || a.acquired != 1 || a.released != 1 {
				t.Fatalf("%+v %v %+v", r, e, a)
			}
		})
	}
}

type startupReleaseFailure struct{}

func (startupReleaseFailure) Acquire(context.Context, engine.Dispatch, Binding, engine.Session) (func() error, error) {
	return func() error { return errors.New("release failed") }, nil
}
func TestStartupReleaseFailureCannotAuthorizeReplay(t *testing.T) {
	x, d, _ := fixture(t)
	x.Runtime = startupRuntime{true, true}
	x.Admission = startupReleaseFailure{}
	r, e := x.Execute(context.Background(), d)
	if e == nil || r.Class != "uncertain" {
		t.Fatalf("%+v %v", r, e)
	}
}

type partialStartupRuntime struct{ calls int }

func (s *partialStartupRuntime) Run(context.Context, runtime.Launch) (runtime.Collected, error) {
	s.calls++
	if s.calls == 1 {
		return runtime.Collected{ProcessesRetired: true, Output: []byte(`{"pass":true}`)}, nil
	}
	return runtime.Collected{NonExecution: true, ProcessesRetired: true}, errors.New("startup failed")
}

// A partly executed review is never a refunded startup retry: its later
// session's start failure keeps the review that ran and is the charged
// later-reviewer class, on which the engine runs every reviewer again
// (FAE-RT-004, FAE-WF-033).
func TestStartupCannotReplayPartlyExecutedReview(t *testing.T) {
	x, d, a := fixture(t)
	x.Runtime = &partialStartupRuntime{}
	d.Config.Primitive = workflow.Review
	d.Sessions = append(d.Sessions, engine.Session{ID: "two", Role: "reviewer"})
	r, e := x.Execute(context.Background(), d)
	if e == nil || r.Class == "startup-retry" || r.Class != "later-reviewer-not-started" || len(r.Reviews) != 1 || r.Reviews[0].Session != "one" || a.released != 2 {
		t.Fatalf("%+v %v %+v", r, e, a)
	}
}

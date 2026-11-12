package execution

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// refusedBeforeLaunchClass is engine.ClassRefusedBeforeLaunch, spelled out so
// the tests pin its wire value.
const refusedBeforeLaunchClass = "refused-before-launch"

// FAE-RT-004 (operator ruling 2026-10-29): every failure before a dispatch's
// first session is admitted is a refusal before launch, which the engine
// charges nothing for and bounds by SameGate: an unresolvable binding, a
// tree that can't be measured, unreadable engine feedback, a review whose
// later reviewer has no route or brief (checked before any reviewer runs, so
// none runs for nothing), and missing executor dependencies. Nothing is
// admitted or launched. Stale residue keeps its class, and a cancellation is
// not a refusal.
func TestRefusedBeforeLaunchTypesEveryFailureBeforeTheFirstLaunch(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		setup      func(context.CancelFunc, *Executor, *engine.Dispatch)
	}{
		{"binding unresolvable", refusedBeforeLaunchClass, func(_ context.CancelFunc, x *Executor, _ *engine.Dispatch) {
			x.Resolve = func(context.Context, engine.Dispatch) (Binding, error) {
				return Binding{}, errors.New("unknown live task")
			}
		}},
		{"tree unmeasurable", refusedBeforeLaunchClass, func(_ context.CancelFunc, x *Executor, _ *engine.Dispatch) {
			x.Tree = func(context.Context, Binding) (string, error) { return "", errors.New("git rev-parse failed") }
		}},
		{"stale residue", engine.ClassStaleResidue, func(_ context.CancelFunc, x *Executor, _ *engine.Dispatch) {
			x.Tree = func(context.Context, Binding) (string, error) { return "", &ResidueError{Detail: "moved"} }
		}},
		{"feedback unreadable", refusedBeforeLaunchClass, func(_ context.CancelFunc, x *Executor, _ *engine.Dispatch) {
			x.Feedback = func(context.Context, engine.Dispatch, workflow.Document) ([]FeedbackRun, error) {
				return nil, errors.New("engine feedback missing")
			}
		}},
		{"later reviewer without a route", refusedBeforeLaunchClass, func(_ context.CancelFunc, x *Executor, d *engine.Dispatch) {
			d.Config.Primitive = workflow.Review
			d.Sessions = append(d.Sessions, engine.Session{ID: "two", Role: "unrouted"})
		}},
		{"later reviewer without a brief for the phase", refusedBeforeLaunchClass, func(_ context.CancelFunc, x *Executor, d *engine.Dispatch) {
			d.Config.Primitive = workflow.Review
			d.Config.Phase = "implement"
			d.Sessions = append(d.Sessions, engine.Session{ID: "two", Role: "briefed"})
		}},
		{"dependencies missing", refusedBeforeLaunchClass, func(_ context.CancelFunc, x *Executor, _ *engine.Dispatch) { x.Tree = nil }},
		{"cancelled", "", func(cancel context.CancelFunc, x *Executor, _ *engine.Dispatch) {
			x.Resolve = func(context.Context, engine.Dispatch) (Binding, error) {
				cancel()
				return Binding{}, context.Canceled
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, d, a := fixture(t)
			resolve := x.Resolve
			x.Resolve = func(ctx context.Context, d engine.Dispatch) (Binding, error) {
				b, err := resolve(ctx, d)
				role := b.Document.Roles["reviewer"]
				briefed := role
				briefed.Briefs = map[string]string{"test-first": "write the test"}
				b.Document.Roles["briefed"] = briefed
				return b, err
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tc.setup(cancel, x, &d)
			r, err := x.Execute(ctx, d)
			if err == nil || r.Class != tc.want || a.acquired != 0 {
				t.Fatalf("class %q err %v acquired %d", r.Class, err, a.acquired)
			}
			if tc.want == refusedBeforeLaunchClass && (r.Detail != err.Error() || r.Session != "one" || r.DispatchID != d.ID || len(r.Reviews) != 0) {
				t.Fatalf("result %+v err %v", r, err)
			}
		})
	}
}

// laterDeferringAdmission admits the first session and refuses the second
// with err.
type laterDeferringAdmission struct {
	admission
	err error
}

func (a *laterDeferringAdmission) Acquire(ctx context.Context, d engine.Dispatch, b Binding, s engine.Session) (func() error, error) {
	if a.acquired == 1 {
		a.acquired++
		return nil, a.err
	}
	return a.admission.Acquire(ctx, d, b, s)
}

// FAE-HOST-006, FAE-RT-004: a later review session whose host admission
// defers it before launch is a typed deferral carrying the review that ran,
// not an uncertain dispatch: the engine parks it without a charge, as it
// parks a first session's deferral. Any other admission failure of a later
// session stays uncertain.
func TestLaterSessionAdmissionDeferralIsATypedDeferral(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		err        error
	}{
		{"deferred", "infrastructure-deferred", laterReviewerDeferral{}},
		{"admission failed", "uncertain", errors.New("admission journal unreadable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, d, _ := fixture(t)
			a := &laterDeferringAdmission{err: tc.err}
			x.Admission = a
			// No wait for admission here: the class is what is checked.
			x.LaterSessionAdmissionWait = time.Nanosecond
			d.Config.Primitive = workflow.Review
			d.Sessions = append(d.Sessions, engine.Session{ID: "two", Role: "reviewer"})
			r, err := x.Execute(context.Background(), d)
			if err == nil || r.Class != tc.want || a.acquired != 2 || a.released != 1 {
				t.Fatalf("class %q err %v acquired %d released %d", r.Class, err, a.acquired, a.released)
			}
			if len(r.Reviews) != 1 || r.Reviews[0].Session != "one" || string(r.Reviews[0].Output) != `{"pass":true}` || len(r.Output) != 0 || r.NewTree != "" {
				t.Fatalf("result %+v: want the first review only", r)
			}
		})
	}
}

// launchRefusedRuntime is a runtime whose launch fails before the bridge
// starts (runtime.LaunchRefused: a session directory, configuration, shim,
// brief or pipe), after any earlier launches ran; it reports whether the
// runtime proves the non-execution, as Direct does.
type launchRefusedRuntime struct {
	calls, failAt int
	proof         bool
}

func (r *launchRefusedRuntime) Run(context.Context, runtime.Launch) (runtime.Collected, error) {
	r.calls++
	if r.calls < r.failAt {
		return runtime.Collected{ProcessesRetired: true, Output: []byte(`{"pass":true}`)}, nil
	}
	return runtime.Collected{ProcessesRetired: true, NonExecution: r.proof}, &runtime.TurnError{Class: "launch-refused", Role: "coder", DispatchID: "dispatch", Cause: errors.New("mkdir session: no space left on device")}
}

// FAE-RT-004, FAE-RT-009 (re-audit 11: Fae's own launch failures were
// recorded as malformed output against the binding and parked
// UncertainDispatch, and the redispatch cost an attempt): the runtime's own
// refusal of a launch before the bridge started is a refusal before launch,
// with no provider health observed, for a first session and for a later
// review session, which carries the review that ran. The runtime's
// statement that nothing started (the launch-refused class) suffices; a
// changed tree is a rejected change, as after the bridge's own refusal
// before the provider.
func TestRuntimeLaunchRefusalIsRefusedBeforeLaunchAndChargesNoProvider(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		failAt     int
		proof      bool
		changed    bool
	}{
		{"first session, with the non-execution proof", refusedBeforeLaunchClass, 1, true, false},
		{"first session, by class alone", refusedBeforeLaunchClass, 1, false, false},
		{"later review session", refusedBeforeLaunchClass, 2, true, false},
		{"tree changed", engine.ClassRejectedChange, 1, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, d, a := fixture(t)
			if tc.failAt > 1 {
				d.Config.Primitive = workflow.Review
				d.Sessions = append(d.Sessions, engine.Session{ID: "two", Role: "reviewer"})
			}
			x.Runtime = &launchRefusedRuntime{failAt: tc.failAt, proof: tc.proof}
			trees := 0
			x.Tree = func(context.Context, Binding) (string, error) {
				if trees++; tc.changed && trees > 1 {
					return "changed", nil
				}
				return "tree", nil
			}
			observed := 0
			x.Observe = func(context.Context, engine.Dispatch, engine.Session, providerhealth.Outcome) error {
				observed++
				return nil
			}
			r, err := x.Execute(context.Background(), d)
			if err == nil || r.Class != tc.want || a.released != tc.failAt || observed != tc.failAt-1 {
				t.Fatalf("class %q err %v released %d observed %d", r.Class, err, a.released, observed)
			}
			if tc.want == refusedBeforeLaunchClass && (!strings.Contains(r.Detail, "launch-refused") || !strings.Contains(r.Detail, "no space left on device") || len(r.Reviews) != tc.failAt-1 || len(r.Output) != 0) {
				t.Fatalf("result %+v", r)
			}
		})
	}
}

// failingLaunchAdmission admits every session and fails to record the
// launch boundary of the nth.
type failingLaunchAdmission struct {
	lifecycleAdmission
	launches, failAt int
}

func (a *failingLaunchAdmission) Launching(ctx context.Context, session string) error {
	if a.launches++; a.launches == a.failAt {
		return errors.New("admission journal: write failed")
	}
	return a.lifecycleAdmission.Launching(ctx, session)
}

// FAE-RT-004 (re-audit 11: a failed Launching write parked
// UncertainDispatch and the redispatch cost an attempt): a launch boundary
// that cannot be recorded launched nothing, so the dispatch is refused
// before launch, its admission released; a later session's carries the
// review that ran.
// failingReleaseAfterNonLaunchAdmission simulates a release or retirement
// write failure after a session was refused before launch.
type failingReleaseAfterNonLaunchAdmission struct {
	failingLaunchAdmission
	failRelease, failRetired bool
}

func (a *failingReleaseAfterNonLaunchAdmission) Acquire(ctx context.Context, d engine.Dispatch, b Binding, s engine.Session) (func() error, error) {
	rel, err := a.failingLaunchAdmission.Acquire(ctx, d, b, s)
	if err != nil {
		return nil, err
	}
	return func() error {
		_ = rel()
		if a.failRelease {
			return errors.New("lease release failed")
		}
		return nil
	}, nil
}

func (a *failingReleaseAfterNonLaunchAdmission) Retired(ctx context.Context, session string, proven bool) error {
	if a.failRetired {
		return errors.New("retirement write failed")
	}
	return a.failingLaunchAdmission.Retired(ctx, session, proven)
}

func TestFailedLaunchBoundaryIsRefusedBeforeLaunch(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		x, d, _ := fixture(t)
		if failAt > 1 {
			d.Config.Primitive = workflow.Review
			d.Sessions = append(d.Sessions, engine.Session{ID: "two", Role: "reviewer"})
		}
		a := &failingLaunchAdmission{failAt: failAt}
		x.Admission = a
		run := &stubRuntime{output: runtime.Collected{Output: []byte(`{"pass":true}`), ProcessesRetired: true}}
		x.Runtime = run
		r, err := x.Execute(context.Background(), d)
		if err == nil || r.Class != refusedBeforeLaunchClass || r.Detail != "admission journal: write failed" || len(run.launches) != failAt-1 || len(r.Reviews) != failAt-1 || a.events[len(a.events)-1] != "release" {
			t.Fatalf("session %d: class %q err %v launches %d reviews %d events %v", failAt, r.Class, err, len(run.launches), len(r.Reviews), a.events)
		}
	}
	// A failed release or retirement write after a proven non-launch must
	// not downgrade refused-before-launch to uncertain.
	for _, tc := range []struct {
		name                             string
		failLaunch, failRelease, failRet bool
	}{
		{"launching failed and release failed", true, true, false},
		{"launch-refused and release failed", false, true, false},
		{"launch-refused and retired failed", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, d, _ := fixture(t)
			a := &failingReleaseAfterNonLaunchAdmission{failRelease: tc.failRelease, failRetired: tc.failRet}
			if tc.failLaunch {
				a.failAt = 1
			}
			x.Admission = a
			x.Runtime = &launchRefusedRuntime{failAt: 1, proof: true}
			r, err := x.Execute(context.Background(), d)
			if err == nil || r.Class != refusedBeforeLaunchClass {
				t.Fatalf("%s: class=%q err=%v", tc.name, r.Class, err)
			}
		})
	}
}

// FAE-HOST-006, FAE-WF-033 (re-audit 11: a later session's deferral reran
// every earlier reviewer, without bound): a later review session whose
// admission is deferred asks again, and runs once admitted, with the
// earlier reviewer not run again.
func TestLaterSessionWaitsForAdmissionInsteadOfRerunningEarlierReviewers(t *testing.T) {
	x, d, _ := fixture(t)
	d.Config.Primitive = workflow.Review
	d.Sessions = append(d.Sessions, engine.Session{ID: "two", Role: "reviewer"})
	a := &laterDeferringAdmission{err: laterReviewerDeferral{}}
	x.Admission = a
	x.LaterSessionAdmissionRetry = time.Millisecond
	run := &stubRuntime{output: runtime.Collected{Output: []byte(`{"pass":true}`), ProcessesRetired: true}}
	x.Runtime = run
	r, err := x.Execute(context.Background(), d)
	if err != nil || len(r.Reviews) != 2 || len(run.launches) != 2 || a.acquired != 3 || a.released != 2 {
		t.Fatalf("err %v reviews %d launches %d acquired %d released %d", err, len(r.Reviews), len(run.launches), a.acquired, a.released)
	}
}

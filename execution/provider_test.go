package execution

import (
	"context"
	"errors"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
	"testing"
	"time"
)

type stubRuntime struct {
	launches []runtime.Launch
	err      error
	output   runtime.Collected
}

func (s *stubRuntime) Run(_ context.Context, l runtime.Launch) (runtime.Collected, error) {
	s.launches = append(s.launches, l)
	return s.output, s.err
}

type deferred struct{}

func (deferred) Error() string           { return "pane cap" }
func (deferred) AdmissionDeferred() bool { return true }

// denyNth defers the nth admission, and every later one when onward is set.
type denyNth struct {
	count, n int
	onward   bool
}

func (a *denyNth) Acquire(context.Context, engine.Dispatch, Binding, engine.Session) (func() error, error) {
	a.count++
	if a.count == a.n || a.onward && a.count > a.n {
		return nil, deferred{}
	}
	return func() error { return nil }, nil
}
func TestProviderObservationAndPinnedRoute(t *testing.T) {
	x, d, _ := fixture(t)
	old := x.Resolve
	x.Resolve = func(ctx context.Context, d engine.Dispatch) (Binding, error) {
		b, e := old(ctx, d)
		role := b.Document.Roles["coder"]
		role.Routes = append(role.Routes, workflow.Binding{Provider: "provider", Model: "fallback"})
		b.Document.Roles["coder"] = role
		return b, e
	}
	d.Sessions[0].Route = workflow.Binding{Provider: "provider", Model: "fallback"}
	runtime := &stubRuntime{output: runtime.Collected{Output: []byte(`{"pass":true}`), ProcessesRetired: true}}
	x.Runtime = runtime
	var outcomes []providerhealth.Outcome
	x.Observe = func(_ context.Context, _ engine.Dispatch, s engine.Session, o providerhealth.Outcome) error {
		if s.Route.Model != "fallback" {
			t.Fatal(s)
		}
		outcomes = append(outcomes, o)
		return nil
	}
	_, e := x.Execute(context.Background(), d)
	if e != nil || len(outcomes) != 1 || outcomes[0] != providerhealth.OK || runtime.launches[0].Binding.Model != "fallback" {
		t.Fatal(outcomes, e)
	}
	runtime.err = context.DeadlineExceeded
	_, e = x.Execute(context.Background(), d)
	if e == nil || outcomes[1] != providerhealth.Timeout {
		t.Fatal(outcomes, e)
	}
	runtime.err = nil
	runtime.output.Output = []byte(`broken`)
	_, e = x.Execute(context.Background(), d)
	if e == nil || outcomes[2] != providerhealth.MalformedOutput {
		t.Fatal(outcomes, e)
	}
	x.Observe = func(context.Context, engine.Dispatch, engine.Session, providerhealth.Outcome) error {
		return errors.New("durability failed")
	}
	runtime.output.Output = []byte(`{"pass":true}`)
	r, e := x.Execute(context.Background(), d)
	if e == nil || r.Class != "uncertain" || len(r.Output) > 0 {
		t.Fatal("accepted without durable health", r, e)
	}
}

// FAE-HOST-006, FAE-RT-004: an admission deferral concerns only the session
// it kept from launching. The first session's defers the dispatch with
// nothing run, at once; a later review session's asks for admission again
// within the executor's wait and, still deferred past it, defers the
// dispatch carrying the review that ran (the engine parks it without a
// charge and runs every reviewer again).
func TestDeferredAdmissionBeforeLaunchOnly(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(string(rune('0'+n)), func(t *testing.T) {
			x, d, _ := fixture(t)
			d.Config.Primitive = workflow.Review
			d.Sessions = append(d.Sessions, engine.Session{ID: "two", Role: "reviewer"})
			a := &denyNth{n: n, onward: true}
			x.Admission = a
			x.LaterSessionAdmissionWait, x.LaterSessionAdmissionRetry = 50*time.Millisecond, 5*time.Millisecond
			run := &stubRuntime{output: runtime.Collected{Output: []byte(`{"pass":true}`), ProcessesRetired: true}}
			x.Runtime = run
			observed := 0
			x.Observe = func(context.Context, engine.Dispatch, engine.Session, providerhealth.Outcome) error {
				observed++
				return nil
			}
			started := time.Now()
			r, e := x.Execute(context.Background(), d)
			if e == nil || r.Class != "infrastructure-deferred" || len(run.launches) != n-1 || observed != n-1 || len(r.Reviews) != n-1 {
				t.Fatal(r, e, len(run.launches), observed)
			}
			// A first session's deferral is not retried; a later session's
			// is, within the wait, and reviewer a is not run again.
			if n == 1 && (a.count != 1 || time.Since(started) > 40*time.Millisecond) || n == 2 && (a.count < 4 || time.Since(started) < 40*time.Millisecond) {
				t.Fatalf("session %d: %d admissions asked in %s", n, a.count, time.Since(started))
			}
		})
	}
}

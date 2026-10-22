package loops

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestFivePersistentIterations(t *testing.T) {
	now := time.Now().UTC()
	s, e := New(Spec{ID: "five", Kind: "loop", Role: "oracle", Prompt: "continue", MaxIterations: 5}, now)
	if e != nil {
		t.Fatal(e)
	}
	for i := int64(1); i <= 5; i++ {
		if !s.Due(now) {
			t.Fatal("not due", s)
		}
		s.InFlight = true
		r, e := Scripted(context.Background(), Request{Spec: s.Spec, SessionID: s.SessionID, Iteration: i, Context: s.Context})
		s.Complete(r, e, now)
		raw, _ := json.Marshal(s)
		if e = json.Unmarshal(raw, &s); e != nil {
			t.Fatal(e)
		}
	}
	if s.State != "completed" || s.Iterations != 5 || s.Due(now) {
		t.Fatal(s)
	}
	var c struct {
		Session string
		Prompts int
	}
	json.Unmarshal(s.Context, &c)
	if c.Session != "loop-five" || c.Prompts != 5 {
		t.Fatal(c)
	}
}
func TestBoundsAndRecovery(t *testing.T) {
	now := time.Now()
	s, _ := New(Spec{ID: "x", Kind: "loop", Role: "oracle", Prompt: "hi", IntervalSeconds: 10}, now)
	if s.Spec.ExpiresAt.Sub(now) != DefaultExpiry {
		t.Fatal(s)
	}
	s.InFlight = true
	if s.Due(now) {
		t.Fatal("duplicate")
	}
	s.Recover()
	if s.State != "uncertain" || s.Due(now) {
		t.Fatal(s)
	}
	s.InFlight = false
	s.State = "active"
	s.Bound(s.Spec.ExpiresAt)
	if s.State != "expired" {
		t.Fatal(s)
	}
	s.State = "active"
	s.Spec.ExpiresAt = now.Add(time.Hour)
	s.Complete(Result{json.RawMessage(`{}`), json.RawMessage(`{}`)}, nil, now)
	if s.Due(now) || !s.Due(now.Add(10*time.Second)) {
		t.Fatal(s)
	}
}
func TestRejectInvalid(t *testing.T) {
	now := time.Now()
	base := Spec{ID: "x", Kind: "loop", Role: "oracle", Prompt: "hi"}
	for _, modify := range []func(*Spec){func(s *Spec) { s.ID = "../x" }, func(s *Spec) { s.Kind = "cron" }, func(s *Spec) { s.Prompt = "" }, func(s *Spec) { s.IntervalSeconds = -1 }, func(s *Spec) { s.MaxIterations = -1 }, func(s *Spec) { s.ExpiresAt = now }, func(s *Spec) { s.ExpiresAt = now.Add(366 * 24 * time.Hour) }} {
		s := base
		modify(&s)
		if _, e := New(s, now); e == nil {
			t.Fatal(s)
		}
	}
	s, _ := New(base, now)
	s.InFlight = true
	s.Complete(Result{}, nil, now)
	if s.State != "uncertain" {
		t.Fatal(s)
	}
}
func TestDispatchPredicate(t *testing.T) {
	for bits := 0; bits < 32; bits++ {
		a, b, c, d, e := bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0, bits&16 != 0
		if CanDispatch(a, b, c, d, e) != (a && !b && c && d && e) {
			t.Fatal(bits)
		}
	}
}

func TestDurableValidation(t *testing.T) {
	s, _ := New(Spec{ID: "durable", Kind: "loop", Role: "oracle", Prompt: "continue"}, time.Now())
	if e := s.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*Schedule){func(s *Schedule) { s.SessionID = "wrong" }, func(s *Schedule) { s.State = "unknown" }, func(s *Schedule) { s.Spec.ExpiresAt = time.Time{} }, func(s *Schedule) { s.Iterations = -1 }, func(s *Schedule) { s.State = "completed" }, func(s *Schedule) { s.Context = json.RawMessage(`bad`) }} {
		copy := s
		mutate(&copy)
		if e := copy.Validate(); e == nil {
			t.Fatal(copy)
		}
	}
}

func TestDeferralRequiresUnjoinedProof(t *testing.T) {
	now := time.Now()
	s, _ := New(Spec{ID: "deferred", Kind: "loop", Role: "oracle", Prompt: "hi"}, now)
	s.InFlight = true
	s.Complete(Result{}, &DeferredError{Cause: errors.New("capacity")}, now)
	if s.State != "active" || s.Iterations != 0 || s.InFlight || s.Due(now) || !s.Due(now.Add(5*time.Second)) {
		t.Fatal(s)
	}
	s.InFlight = true
	s.Complete(Result{}, errors.Join(&DeferredError{Cause: errors.New("capacity")}, errors.New("release failed")), now)
	if s.State != "uncertain" {
		t.Fatal("resource uncertainty accepted as safe deferral", s)
	}
}

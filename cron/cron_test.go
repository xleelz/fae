package cron

import (
	"testing"
	"time"
)

func spec() Spec {
	return Spec{ID: "daily", Kind: "cron", Anchor: time.Date(2026, 10, 18, 10, 0, 0, 123, time.UTC), IntervalSeconds: 60, Template: Template{WorkflowID: "default", Revision: 1}}
}
func TestAnchoredDueAndExclusiveEnd(t *testing.T) {
	p := spec()
	p.End = p.Anchor.Add(121 * time.Second)
	s, e := New(p)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		offset time.Duration
		want   int
	}{{-time.Nanosecond, 0}, {0, 1}, {59 * time.Second, 1}, {60 * time.Second, 2}, {120 * time.Second, 3}, {time.Hour, 3}} {
		skip, got := s.Due(p.Anchor.Add(tc.offset))
		if skip != 0 || len(got) != tc.want {
			t.Fatal(tc, skip, got)
		}
	}
	s.Next = 3
	s.Admitted = 3
	s.Bound()
	if s.State != "completed" {
		t.Fatal(s)
	}
}
func TestBoundedCatchUpAndPause(t *testing.T) {
	p := spec()
	p.CatchUpLimit = 3
	s, _ := New(p)
	skip, indices := s.Due(p.Anchor.Add(600 * time.Second))
	if skip != 8 || len(indices) != 3 || indices[0] != 8 || indices[2] != 10 {
		t.Fatal(skip, indices)
	}
	s.Next += skip
	s.Skipped += skip
	s.State = "paused"
	if _, got := s.Due(p.Anchor.Add(time.Hour)); len(got) != 0 {
		t.Fatal(got)
	}
	s.State = "active"
	s.Next++
	s.Admitted++
	if e := s.Validate(); e != nil {
		t.Fatal(e)
	}
	if s.TaskID(0) == s.TaskID(1) || len(s.TaskID(0)) > 80 {
		t.Fatal("occurrence identity")
	}
}
func TestCronValidation(t *testing.T) {
	for _, edit := range []func(*Spec){func(p *Spec) { p.ID = "../bad" }, func(p *Spec) { p.Kind = "loop" }, func(p *Spec) { p.IntervalSeconds = 0 }, func(p *Spec) { p.IntervalSeconds = 31536001 }, func(p *Spec) { p.Anchor = time.Time{} }, func(p *Spec) { p.End = p.Anchor }, func(p *Spec) { p.Template.Revision = 0 }, func(p *Spec) { p.Template.Mode = "Normal" }, func(p *Spec) { p.CatchUpLimit = 101 }} {
		p := spec()
		edit(&p)
		if _, e := New(p); e == nil {
			t.Fatal(p)
		}
	}
	s, _ := New(spec())
	s.Next = 1
	if s.Validate() == nil {
		t.Fatal("corrupt accounting accepted")
	}
	s.Next = 0
	s.State = "unknown"
	if s.Validate() == nil {
		t.Fatal("corrupt lifecycle accepted")
	}
}
func TestCronTimestampBound(t *testing.T) {
	p := spec()
	p.Anchor = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	s, _ := New(p)
	if _, ok := s.At(1); ok {
		t.Fatal("timestamp overflow")
	}
	s.Next = 1
	s.Admitted = 1
	s.Bound()
	if s.State != "completed" {
		t.Fatal(s)
	}
}
func TestCanAdmit(t *testing.T) {
	for bits := 0; bits < 16; bits++ {
		a, b, c, d := bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0
		if CanAdmit(a, b, c, d) != (a && b && c && d) {
			t.Fatal(bits)
		}
	}
}

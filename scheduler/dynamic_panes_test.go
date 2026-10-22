package scheduler

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestDynamicPaneReductionPreservesReservationsAndResumes(t *testing.T) {
	ctx := context.Background()
	p, r, c := paneFixture(t, 4)
	if err := p.Configure(ctx, "instance", 4); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		r.ID = fmt.Sprint(i)
		if err := p.Reserve(ctx, r, noPanes, healthyPaneSample, c, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Configure(ctx, "instance", 2); err != nil {
		t.Fatal(err)
	}
	rows, err := p.Snapshot(ctx)
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
	r.ID = "new"
	if err := p.Reserve(ctx, r, noPanes, healthyPaneSample, c, time.Now()); err == nil {
		t.Fatal("reduction admitted excess pane")
	}
	if err := p.Configure(ctx, "instance", 5); err != nil {
		t.Fatal(err)
	}
	if err := p.Reserve(ctx, r, noPanes, healthyPaneSample, c, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A newly opened handle with old startup config must use the shared update.
	reopened, err := OpenPaneLedger(p.ledger.path, 4)
	if err != nil {
		t.Fatal(err)
	}
	n, err := reopened.Limit(ctx)
	if err != nil || n != 5 {
		t.Fatal(n, err)
	}
}
func TestDynamicPaneSharedLimitsCannotOverwriteOtherInstance(t *testing.T) {
	ctx := context.Background()
	p, _, _ := paneFixture(t, 60)
	for instance, n := range map[string]int{"first": 4, "second": 8} {
		if err := p.Configure(ctx, instance, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Configure(ctx, "second", 100); err != nil {
		t.Fatal(err)
	}
	n, err := p.Limit(ctx)
	if err != nil || n != 4 {
		t.Fatal(n, err)
	}
	if err := p.Configure(ctx, "first", 9); err != nil {
		t.Fatal(err)
	}
	n, err = p.Limit(ctx)
	if err != nil || n != 9 {
		t.Fatal(n, err)
	}
	for _, bad := range []int{-1, 0, 1, 1000001} {
		if err := p.Configure(ctx, "first", bad); err == nil {
			t.Fatal("invalid limit accepted", bad)
		}
	}
}
func TestTightenPaneLimit(t *testing.T) {
	for a := 0; a < 10; a++ {
		for b := 0; b < 10; b++ {
			n := TightenPaneLimit(a, b)
			if n > a || n > b || (n != a && n != b) {
				t.Fatal(a, b, n)
			}
		}
	}
}

func TestDynamicPaneDeadInstanceDoesNotClampHost(t *testing.T) {
	ctx := context.Background()
	p, _, _ := paneFixture(t, 60)
	if err := p.Configure(ctx, "live", 40); err != nil {
		t.Fatal(err)
	}
	if err := p.ledger.transact(ctx, true, func(s *ledgerState) error {
		// Reused PID with distinct exact birth is proof the registering owner died.
		current := s.PaneLimits["live"]
		current.Limit = 2
		current.Birth = "old-owner-birth"
		s.PaneLimits["dead"] = current
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	n, err := p.Limit(ctx)
	if err != nil || n != 40 {
		t.Fatal(n, err)
	}
}

func TestDynamicPaneMalformedRegistrationFailsClosed(t *testing.T) {
	for _, kind := range []string{"empty-key", "pid", "started", "birth", "limit"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			p, _, _ := paneFixture(t, 60)
			if err := p.Configure(ctx, "instance", 40); err != nil {
				t.Fatal(err)
			}
			if err := p.ledger.transact(ctx, true, func(s *ledgerState) error {
				r := s.PaneLimits["instance"]
				key := "instance"
				switch kind {
				case "empty-key":
					key = ""
				case "pid":
					r.Owner.PID = 0
				case "started":
					r.Owner.Started = 0
				case "birth":
					r.Birth = ""
				case "limit":
					r.Limit = 0
				}
				s.PaneLimits[key] = r
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Limit(ctx); err == nil {
				t.Fatal("malformed config accepted")
			}
			if err := p.Configure(ctx, "instance", 50); err == nil {
				t.Fatal("malformed config overwritten")
			}
		})
	}
}

func TestDynamicPaneDifferentLiveOwnerCannotBeOverwritten(t *testing.T) {
	ctx := context.Background()
	p, _, _ := paneFixture(t, 60)
	if err := p.Configure(ctx, "instance", 40); err != nil {
		t.Fatal(err)
	}
	if err := p.ledger.transact(ctx, true, func(s *ledgerState) error {
		r := s.PaneLimits["instance"]
		r.Owner.Started++
		s.PaneLimits["instance"] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Configure(ctx, "instance", 50); err == nil {
		t.Fatal("different live owner overwritten")
	}
	if n, err := p.Limit(ctx); err != nil || n != 40 {
		t.Fatal(n, err)
	}
}

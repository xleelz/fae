package scheduler

import (
	"context"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/processowner"
	"path/filepath"
	"testing"
	"time"
)

func TestDynamicHeavyDrainPreservesHoldersAndQueue(t *testing.T) {
	ctx := context.Background()
	l, e := OpenDynamicLedger(filepath.Join(physicalTemp(t), "host"), 4)
	if e != nil {
		t.Fatal(e)
	}
	cfg := hostguard.DefaultConfig()
	if e = l.ConfigureHost(ctx, "a", 4, cfg); e != nil {
		t.Fatal(e)
	}
	o, _ := CurrentOwner()
	now := time.Now()
	if e = l.Enqueue(ctx, Request{ID: "held", Owner: o, Weight: 3, Queued: now}); e != nil {
		t.Fatal(e)
	}
	if g, _, e := l.Advance(ctx, now, map[Owner]Liveness{}); e != nil || len(g) != 1 {
		t.Fatal(g, e)
	}
	if e = l.Enqueue(ctx, Request{ID: "next", Owner: o, Weight: 2, Queued: now}); e != nil {
		t.Fatal(e)
	}
	if e = l.ConfigureHost(ctx, "a", 1, cfg); e != nil {
		t.Fatal(e)
	}
	p, e := l.Snapshot(ctx)
	if e != nil || len(p.Holders) != 1 || len(p.Waiting) != 1 || p.AdmissionLimit != 1 {
		t.Fatal(p, e)
	}
	if g, _, e := l.Advance(ctx, now, map[Owner]Liveness{}); e != nil || len(g) != 0 {
		t.Fatal(g, e)
	}
	if e = l.Release(ctx, "held", o, "retired"); e != nil {
		t.Fatal(e)
	}
	if g, _, e := l.Advance(ctx, now, map[Owner]Liveness{}); e != nil || len(g) != 0 {
		t.Fatal(g, e)
	}
	if e = l.ConfigureHost(ctx, "a", 2, cfg); e != nil {
		t.Fatal(e)
	}
	if g, _, e := l.Advance(ctx, now, map[Owner]Liveness{}); e != nil || len(g) != 1 {
		t.Fatal(g, e)
	}
	reopened, e := OpenDynamicLedger(l.path, 99)
	if e != nil {
		t.Fatal(e)
	}
	n, _, e := reopened.HostLimits(ctx)
	if e != nil || n != 2 {
		t.Fatal(n, e)
	}
}
func TestDynamicHostSharedConservativeAndExactUnconfigure(t *testing.T) {
	ctx := context.Background()
	l, e := OpenDynamicLedger(filepath.Join(physicalTemp(t), "host"), 4)
	if e != nil {
		t.Fatal(e)
	}
	a := hostguard.DefaultConfig()
	b := a
	b.Soft += hostguard.GiB
	b.Load = 2
	if e = l.ConfigureHost(ctx, "a", 4, a); e != nil {
		t.Fatal(e)
	}
	if e = l.ConfigureHost(ctx, "b", 2, b); e != nil {
		t.Fatal(e)
	}
	n, c, e := l.HostLimits(ctx)
	if e != nil || n != 2 || c.Soft != b.Soft || c.Load != 2 {
		t.Fatal(n, c, e)
	}
	o, _ := CurrentOwner()
	birth, _ := processowner.Birth(o.PID)
	if e = l.UnconfigureHost(ctx, "b", o, "wrong"); e == nil {
		t.Fatal("owner mismatch accepted")
	}
	if e = l.UnconfigureHost(ctx, "b", o, birth); e != nil {
		t.Fatal(e)
	}
	n, c, e = l.HostLimits(ctx)
	if e != nil || n != 4 || c != a {
		t.Fatal(n, c, e)
	}
	if e = l.transact(ctx, true, func(s *ledgerState) error { r := s.HostLimits["a"]; r.Capacity = -1; s.HostLimits["a"] = r; return nil }); e != nil {
		t.Fatal(e)
	}
	if _, _, e = l.HostLimits(ctx); e == nil {
		t.Fatal("corrupt registration accepted")
	}
}
func TestDynamicAdmissionGuard(t *testing.T) {
	for limit := 1; limit <= 4; limit++ {
		for used := 0; used <= 6; used++ {
			for weight := -1; weight <= 6; weight++ {
				want := weight > 0 && used+weight <= limit
				if CanAcquireDynamic(limit, used, weight) != want {
					t.Fatal(limit, used, weight)
				}
			}
		}
	}
}

func TestDynamicHostDeadOwnerAndLiveOwnerRefusal(t *testing.T) {
	ctx := context.Background()
	l, e := OpenDynamicLedger(filepath.Join(physicalTemp(t), "host"), 4)
	if e != nil {
		t.Fatal(e)
	}
	cfg := hostguard.DefaultConfig()
	if e = l.ConfigureHost(ctx, "a", 2, cfg); e != nil {
		t.Fatal(e)
	}
	// PID reuse is proof the old registration is dead, even when that PID is live.
	if e = l.transact(ctx, true, func(s *ledgerState) error {
		r := s.HostLimits["a"]
		r.Birth = "stale-birth"
		s.HostLimits["a"] = r
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if e = l.ConfigureHost(ctx, "a", 3, cfg); e != nil {
		t.Fatal(e)
	}
	// Wrong Started with the same live birth is uncertain, never permission to replace.
	if e = l.transact(ctx, true, func(s *ledgerState) error {
		r := s.HostLimits["a"]
		r.Owner.Started--
		s.HostLimits["a"] = r
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if e = l.ConfigureHost(ctx, "a", 4, cfg); e == nil {
		t.Fatal("replaced live owner's policy")
	}
	n, _, e := l.HostLimits(ctx)
	if e != nil || n != 3 {
		t.Fatal(n, e)
	}
}
func TestDynamicPoolRejectsMalformedLimit(t *testing.T) {
	p, _ := NewPool(2)
	for _, n := range []int{-1, 3} {
		p.AdmissionLimit = n
		if p.Validate() == nil {
			t.Fatal(n)
		}
	}
}

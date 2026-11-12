package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func ownerProbe(states map[Owner]Liveness) func(Owner) Liveness {
	return func(o Owner) Liveness { return states[o] }
}

func TestPoolReclaimDeadOwnersKeepsAliveAndUnknown(t *testing.T) {
	alive, dead, unknown := Owner{1, 1}, Owner{2, 2}, Owner{3, 3}
	now := time.Now()
	p, _ := NewPool(4)
	for i, o := range []Owner{alive, dead, unknown, dead} {
		if e := p.Enqueue(Request{ID: string(rune('a' + i)), Owner: o, Weight: 1, Queued: now.Add(time.Duration(i) * time.Second)}); e != nil {
			t.Fatal(e)
		}
	}
	if _, _, e := p.Advance(now, func(Owner) bool { return true }); e != nil || len(p.Holders) != 4 {
		t.Fatal(p.Holders, e)
	}
	if e := p.Enqueue(Request{ID: "w-dead", Owner: dead, Weight: 1, Queued: now}); e != nil {
		t.Fatal(e)
	}
	if e := p.Enqueue(Request{ID: "w-alive", Owner: alive, Weight: 1, Queued: now}); e != nil {
		t.Fatal(e)
	}
	r := p.ReclaimDeadOwners(ownerProbe(map[Owner]Liveness{alive: Alive, dead: Dead, unknown: Unknown}))
	got := map[string]string{}
	for _, e := range r.Reclaimed {
		got[e.ID] = "reclaimed:" + e.Kind + ":" + e.Liveness
	}
	for _, e := range r.Kept {
		got[e.ID] = "kept:" + e.Kind + ":" + e.Liveness
	}
	want := map[string]string{"a": "kept:holder:alive", "b": "reclaimed:holder:dead", "c": "kept:holder:unknown", "d": "reclaimed:holder:dead", "w-dead": "reclaimed:waiter:dead", "w-alive": "kept:waiter:alive"}
	if len(got) != len(want) {
		t.Fatal(got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatal(k, got[k], v)
		}
	}
	if len(p.Holders) != 2 || len(p.Waiting) != 1 || p.Waiting[0].ID != "w-alive" {
		t.Fatal("reclaim granted or dropped the wrong entries", p.Holders, p.Waiting)
	}
}

func TestLedgerReclaimDeadDryRunAuditAndUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host")
	l, e := OpenLedger(path, 2)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	alive, dead := Owner{10, 10}, Owner{20, 20}
	for id, o := range map[string]Owner{"live": alive, "gone": dead} {
		if e = l.Enqueue(ctx, Request{ID: id, Owner: o, Weight: 1, Queued: time.Now().UTC()}); e != nil {
			t.Fatal(e)
		}
	}
	if granted, _, e := l.Advance(ctx, time.Now().UTC(), map[Owner]Liveness{alive: Alive, dead: Alive}); e != nil || len(granted) != 2 {
		t.Fatal(granted, e)
	}
	probe := ownerProbe(map[Owner]Liveness{alive: Alive, dead: Dead})
	r, e := l.ReclaimDead(ctx, probe, true)
	if e != nil || len(r.Reclaimed) != 1 || r.Reclaimed[0].ID != "gone" || len(r.Kept) != 1 {
		t.Fatal(r, e)
	}
	if p, _ := l.Snapshot(ctx); len(p.Holders) != 2 {
		t.Fatal("dry run wrote the ledger", p.Holders)
	}
	if r, e = l.ReclaimDead(ctx, probe, false); e != nil || len(r.Reclaimed) != 1 {
		t.Fatal(r, e)
	}
	p, _ := l.Snapshot(ctx)
	audit, _ := l.Audit(ctx)
	if len(p.Holders) != 1 || p.Holders[0].ID != "live" || audit[len(audit)-1].Action != "reclaimed" || audit[len(audit)-1].ID != "gone" {
		t.Fatal(p.Holders, audit)
	}
	if r, e = l.ReclaimDead(ctx, probe, false); e != nil || len(r.Reclaimed) != 0 || len(r.Kept) != 1 {
		t.Fatal("reclaim not idempotent", r, e)
	}
	missing := &Ledger{path: filepath.Join(t.TempDir(), "absent"), capacity: 2}
	if _, e = missing.ReclaimDead(ctx, probe, true); e == nil {
		t.Fatal("unreadable ledger did not fail closed")
	}
}

func TestPaneReclaimDeadDryRunAndApply(t *testing.T) {
	p, r, _ := paneFixture(t, 10)
	ctx := context.Background()
	e := p.ledger.transact(ctx, true, func(s *ledgerState) error {
		gone := r
		gone.ID, gone.Owner = "gone", Owner{PID: 7, Started: 7}
		s.Panes = append(s.Panes, r, gone)
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	p.probe = func(x PaneReservation) bool { return x.ID == "gone" }
	report, e := p.ReclaimDead(ctx, true)
	if e != nil || len(report.Reclaimed) != 1 || report.Reclaimed[0].ID != "gone" || len(report.Kept) != 1 {
		t.Fatal(report, e)
	}
	var count int
	_ = p.ledger.transact(ctx, false, func(s *ledgerState) error { count = len(s.Panes); return nil })
	if count != 2 {
		t.Fatal("dry run removed panes")
	}
	if report, e = p.ReclaimDead(ctx, false); e != nil || len(report.Reclaimed) != 1 {
		t.Fatal(report, e)
	}
	_ = p.ledger.transact(ctx, false, func(s *ledgerState) error { count = len(s.Panes); return nil })
	if count != 1 {
		t.Fatal("dead pane kept", count)
	}
}

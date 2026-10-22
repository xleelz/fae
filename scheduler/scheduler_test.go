package scheduler

import (
	"github.com/xleelz/fae/domain"
	"math"
	"testing"
	"time"
)

func TestCapacityExhaustive(t *testing.T) {
	for c := 0; c < 30; c++ {
		for u := 0; u <= c; u++ {
			for w := -2; w < 40; w++ {
				if got := CanAcquire(c, u, w); got != (w > 0 && u+w <= c) {
					t.Fatalf("%d %d %d", c, u, w)
				}
			}
		}
	}
	if CanAcquire(math.MaxInt, math.MaxInt, 1) {
		t.Fatal("overflow")
	}
}
func TestReadyDependencies(t *testing.T) {
	for _, tc := range []struct {
		s    domain.State
		r    string
		want bool
	}{{domain.Done, "", true}, {domain.Retired, "contained", true}, {domain.Retired, "obsolete", false}, {domain.Cancelled, "", false}, {domain.Escalated, "", false}} {
		a := Item{ID: "a", State: tc.s, RetireReason: tc.r}
		b := Item{ID: "b", State: domain.Pending}
		ready, held := Ready([]Item{a, b}, []Edge{{"a", "b", true}})
		if (len(ready) == 1) != tc.want || (!tc.want && len(held) != 1) {
			t.Fatal(tc, ready, held)
		}
	}
}
func TestReadyOrderAndSuccessor(t *testing.T) {
	now := time.Now()
	items := []Item{{ID: "b", State: domain.Pending, Priority: 1, Admitted: now}, {ID: "c", State: domain.Pending, Priority: 2, Admitted: now}, {ID: "a", State: domain.Retired, RetireReason: "superseded-by", Successor: "z"}, {ID: "z", State: domain.Done}}
	r, _ := Ready(items, []Edge{{"a", "b", true}})
	if len(r) != 2 || r[0].ID != "c" {
		t.Fatal(r)
	}
	items[0].Priority = 3
	r, _ = Ready(items, nil)
	if r[0].ID != "b" {
		t.Fatal(r)
	}
	items[2].Successor = "a"
	r, h := Ready(items, []Edge{{"a", "b", true}})
	if len(r) != 1 || len(h) != 1 {
		t.Fatal(r, h)
	}
}
func TestFIFOWeightedAndOwnership(t *testing.T) {
	p, _ := NewPool(2)
	now := time.Now()
	owner := Owner{1, 1}
	add := func(id string, w, cl int, at time.Time) {
		t.Helper()
		if e := p.Enqueue(Request{ID: id, Owner: owner, Weight: w, Class: cl, Queued: at}); e != nil {
			t.Fatal(e)
		}
	}
	alive := func(Owner) bool { return true }
	add("holder", 1, 0, now)
	g, _, e := p.Advance(now, alive)
	if e != nil || len(g) != 1 {
		t.Fatal(g, e)
	}
	add("old", 2, 0, now.Add(time.Second))
	add("new", 1, 2, now.Add(2*time.Second))
	g, _, _ = p.Advance(now.Add(3*time.Second), alive)
	if len(g) != 0 {
		t.Fatal("overtake", g)
	}
	if p.Release("holder", Owner{1, 2}) {
		t.Fatal("pid reuse")
	}
	p.Release("holder", owner)
	g, _, _ = p.Advance(now.Add(4*time.Second), alive)
	if len(g) != 1 || g[0].ID != "old" {
		t.Fatal(g)
	}
	p.Release("old", owner)
	g, _, _ = p.Advance(now.Add(5*time.Second), alive)
	if len(g) != 1 || g[0].ID != "new" {
		t.Fatal(g)
	}
}
func TestWaitWarningTimeoutReclaimAndTies(t *testing.T) {
	p, _ := NewPool(1)
	now := time.Now()
	o := Owner{1, 1}
	alive := func(Owner) bool { return true }
	p.Enqueue(Request{ID: "h", Owner: o, Weight: 1, Queued: now})
	p.Advance(now, alive)
	p.Enqueue(Request{ID: "w", Owner: o, Weight: 1, Queued: now})
	_, ev, _ := p.Advance(now.Add(300*time.Second), alive)
	if len(ev) != 1 || ev[0].Kind != "Waiting" || len(ev[0].Holders) != 1 {
		t.Fatal(ev)
	}
	_, ev, _ = p.Advance(now.Add(301*time.Second), alive)
	if len(ev) != 0 {
		t.Fatal(ev)
	}
	_, ev, _ = p.Advance(now.Add(1800*time.Second), alive)
	if len(ev) != 1 || ev[0].Kind != "Timeout" {
		t.Fatal(ev)
	}
	_, ev, _ = p.Advance(now, func(Owner) bool { return false })
	if len(ev) != 1 || len(p.Holders) != 0 {
		t.Fatal(ev)
	}
	for c := 0; c < 3; c++ {
		p.Enqueue(Request{ID: string(rune('a' + c)), Owner: o, Weight: 1, Class: c, Queued: now})
	}
	g, _, _ := p.Advance(now, alive)
	if len(g) != 1 || g[0].Class != 2 {
		t.Fatal(g)
	}
	if _, _, e := p.Advance(now, nil); e == nil {
		t.Fatal("ledger must fail closed")
	}
}
func TestRolePoolScopes(t *testing.T) {
	old := RoleRevision{"wf", "m1", "oracle", 1, 1, true, true}
	newer := RoleRevision{"wf", "m2", "oracle", 2, 2, true, true}
	key, c, e := RolePool(old, []RoleRevision{newer})
	if e != nil || key != "wf/oracle" || c != 2 {
		t.Fatal(key, c, e)
	}
	newer.Live = false
	_, c, _ = RolePool(old, []RoleRevision{newer})
	if c != 1 {
		t.Fatal(c)
	}
	old.Global = false
	key, _, _ = RolePool(old, nil)
	if key != "wf/m1/oracle" {
		t.Fatal(key)
	}
}
func TestInvalidRequests(t *testing.T) {
	if _, e := NewPool(0); e == nil {
		t.Fatal("zero cap")
	}
	p, _ := NewPool(1)
	if p.Enqueue(Request{}) == nil {
		t.Fatal("invalid accepted")
	}
	r := Request{ID: "x", Owner: Owner{1, 1}, Weight: 1}
	if p.Enqueue(r) != nil || p.Enqueue(r) == nil {
		t.Fatal("duplicate")
	}
	if !WithinLimit(100, -1) || WithinLimit(1, 1) || WithinLimit(-1, -1) {
		t.Fatal("limits")
	}
}

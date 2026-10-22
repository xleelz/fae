package scheduler

import (
	"reflect"
	"testing"
	"time"
)

func TestCorruptLedgerPreservesState(t *testing.T) {
	p, _ := NewPool(2)
	now := time.Now()
	p.Holders = []Lease{{Request{ID: "dead", Owner: Owner{1, 1}, Weight: 1}, now.Add(time.Hour)}, {Request{ID: "live", Owner: Owner{2, 1}, Weight: 1}, now.Add(time.Hour)}, {Request{ID: "corrupt", Owner: Owner{2, 1}, Weight: 3}, now.Add(time.Hour)}}
	before := append([]Lease(nil), p.Holders...)
	_, _, e := p.Advance(now, func(o Owner) bool { return o.PID == 2 })
	if e == nil || !reflect.DeepEqual(before, p.Holders) {
		t.Fatalf("corrupt ledger changed: %v %v", e, p.Holders)
	}
}

package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/hostguard"
)

func paneFixture(t *testing.T, limit int) (*PaneLedger, PaneReservation, hostguard.Config) {
	t.Helper()
	p, e := OpenPaneLedger(filepath.Join(t.TempDir(), "panes"), limit)
	if e != nil {
		t.Fatal(e)
	}
	p.probe = func(PaneReservation) bool { return false }
	cfg := hostguard.DefaultConfig()
	cfg.PaneLimit = limit
	return p, PaneReservation{ID: "one", Socket: "/pending/one", Birth: "exact-birth", Owner: Owner{PID: 123, Started: 123}, Reserved: time.Now().UTC()}, cfg
}
func healthyPaneSample(context.Context) (hostguard.Sample, error) {
	return hostguard.Sample{Memory: hostguard.Reading{Value: 100 * float64(hostguard.GiB), Known: true}, Swap: hostguard.Reading{Value: 1, Known: true}}, nil
}
func noPanes(context.Context) (map[string]int, error) { return map[string]int{}, nil }
func TestPaneLiveInventoryOverridesEmptyLedger(t *testing.T) {
	p, r, c := paneFixture(t, 10)
	e := p.Reserve(context.Background(), r, func(context.Context) (map[string]int, error) { return map[string]int{"/foreign/socket": 10}, nil }, healthyPaneSample, c, time.Now())
	var refused *AdmissionRefusal
	if !errors.As(e, &refused) || refused.Reason != "PaneLimit" {
		t.Fatal(e)
	}
	rows, e := p.Snapshot(context.Background())
	if e != nil || len(rows) != 0 {
		t.Fatal(rows, e)
	}
}
func TestPaneConcurrentReservationsAndLiveReplacement(t *testing.T) {
	p, r, c := paneFixture(t, 2)
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			request := r
			request.ID = string(rune('a' + i))
			e := p.Reserve(ctx, request, noPanes, healthyPaneSample, c, time.Now())
			if e == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if success != 2 {
		t.Fatal(success)
	}
	rows, _ := p.Snapshot(ctx)
	for _, row := range rows {
		if e := p.Release(ctx, row.ID, row.Owner, row.Birth); e != nil {
			t.Fatal(e)
		}
	}
	if e := p.Reserve(ctx, r, noPanes, healthyPaneSample, c, time.Now()); e != nil {
		t.Fatal(e)
	}
	if e := p.Bind(ctx, r.ID, r.Owner, r.Birth, "/actual/socket"); e != nil {
		t.Fatal(e)
	}
	r.ID = "second"
	e := p.Reserve(ctx, r, func(context.Context) (map[string]int, error) { return map[string]int{"/actual/socket": 1}, nil }, healthyPaneSample, c, time.Now())
	if e != nil {
		t.Fatal("double-counted live pane", e)
	}
}
func TestPaneMemoryWarmupAndNeverStartedReservation(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "live"}[present], func(t *testing.T) {
			p, r, c := paneFixture(t, 10)
			ctx := context.Background()
			now := time.Now()
			r.Reserved = now
			c.Soft = 0
			c.Hard = 0
			c.HardNoSwap = 0
			c.PerPane = 100
			observe := func(context.Context) (hostguard.Sample, error) {
				return hostguard.Sample{Memory: hostguard.Reading{Value: 150, Known: true}, Swap: hostguard.Reading{Value: 1, Known: true}}, nil
			}
			if e := p.Reserve(ctx, r, noPanes, observe, c, now); e != nil {
				t.Fatal(e)
			}
			inventory := func(context.Context) (map[string]int, error) {
				if present {
					return map[string]int{r.Socket: 1}, nil
				}
				return map[string]int{}, nil
			}
			second := r
			second.ID = "second"
			e := p.Reserve(ctx, second, inventory, observe, c, now.Add(179*time.Second))
			var refused *AdmissionRefusal
			if !errors.As(e, &refused) || refused.Reason != "MemoryReservation" {
				t.Fatal(e)
			}
			e = p.Reserve(ctx, second, inventory, observe, c, now.Add(359*time.Second))
			if present && e != nil {
				t.Fatal(e)
			}
			if !present && e == nil {
				t.Fatal("pending memory prematurely expired")
			}
		})
	}
}
func TestPaneHardLimitBeforeInventoryAndFailuresDoNotReserve(t *testing.T) {
	p, r, c := paneFixture(t, 10)
	called := false
	inv := func(context.Context) (map[string]int, error) {
		called = true
		return nil, errors.New("inventory unavailable")
	}
	observe := func(context.Context) (hostguard.Sample, error) {
		return hostguard.Sample{Memory: hostguard.Reading{Value: 1, Known: true}}, nil
	}
	if e := p.Reserve(context.Background(), r, inv, observe, c, time.Now()); e == nil || called {
		t.Fatal(e, called)
	}
	if e := p.Reserve(context.Background(), r, inv, healthyPaneSample, c, time.Now()); e == nil {
		t.Fatal("unknown inventory admitted")
	}
	rows, _ := p.Snapshot(context.Background())
	if len(rows) != 0 {
		t.Fatal(rows)
	}
}
func TestPaneOwnerRecoveryAndCorruptionFailClosed(t *testing.T) {
	p, r, c := paneFixture(t, 2)
	ctx := context.Background()
	if e := p.Reserve(ctx, r, noPanes, healthyPaneSample, c, time.Now()); e != nil {
		t.Fatal(e)
	}
	foreign := r.Owner
	foreign.Started++
	if e := p.Release(ctx, r.ID, foreign, r.Birth); e == nil {
		t.Fatal("foreign release")
	}
	if e := p.Release(ctx, r.ID, r.Owner, "reused-pid"); e == nil {
		t.Fatal("birth mismatch release")
	}
	if e := p.Bind(ctx, r.ID, foreign, r.Birth, "/socket"); e == nil {
		t.Fatal("foreign bind")
	}
	p.probe = func(got PaneReservation) bool { return got.Birth == r.Birth }
	rows, e := p.Snapshot(ctx)
	if e != nil || len(rows) != 0 {
		t.Fatal(rows, e)
	}
	audits, _ := p.ledger.Audit(ctx)
	if audits[len(audits)-1].Action != "pane-recovered" {
		t.Fatal(audits)
	}
	if e := os.WriteFile(filepath.Join(p.ledger.path, "state.json"), []byte("broken"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Snapshot(ctx); e == nil {
		t.Fatal("corrupt pane ledger accepted")
	}
}
func TestCanReservePaneBoundaries(t *testing.T) {
	for _, x := range []struct {
		limit, live, pending int
		want                 bool
	}{{10, 10, 0, false}, {10, 9, 0, true}, {10, 9, 1, false}, {10, 0, 10, false}, {int(^uint(0) >> 1), 1, int(^uint(0) >> 1), false}} {
		if got := CanReservePane(x.limit, x.live, x.pending); got != x.want {
			t.Fatal(x, got)
		}
	}
}

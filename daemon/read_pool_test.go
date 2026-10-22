package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/api"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadPoolCoalescesIndependentCancellation(t *testing.T) {
	var p readPool
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	query := func(ctx context.Context) (any, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return 42, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, e := p.run(ctx, "same", query); first <- e }()
	<-started
	second := make(chan error, 1)
	go func() {
		v, e := p.run(context.Background(), "same", query)
		if e == nil && v != 42 {
			e = errors.New("wrong result")
		}
		second <- e
	}()
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		n := p.flights["same"].waiters
		p.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter not joined")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if !errors.Is(<-first, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	close(release)
	if e := <-second; e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 1 {
		t.Fatal("not coalesced")
	}
}
func TestReadPoolBoundsAndCancelsLastWaiter(t *testing.T) {
	var p readPool
	var active, peak atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	query := func(ctx context.Context) (any, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if old >= n || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); p.run(ctx, string(rune(i)), query) }(i)
	}
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		n := len(p.flights)
		p.mu.Unlock()
		if n == 64 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queue not filled")
		}
		time.Sleep(time.Millisecond)
	}
	_, err := p.run(ctx, "overflow", query)
	var rpc *api.RPCError
	if !errors.As(err, &rpc) || rpc.Code != "read-busy" {
		t.Fatalf("%v", err)
	}
	cancel()
	wg.Wait()
	if peak.Load() > 8 {
		t.Fatal("pool exceeded", peak.Load())
	}
}
func TestReadPoolDeadlineDoesNotPinCaller(t *testing.T) {
	var p readPool
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := p.run(ctx, "slow", func(context.Context) (any, error) { <-release; return nil, nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestBoardSnapshotAgeAndStaleness(t *testing.T) {
	d := &Daemon{}
	d.status.Store(statusSnapshot{Updated: time.Now().Add(-11 * time.Second)})
	_, err := d.board(nil)
	var rpc *api.RPCError
	if !errors.As(err, &rpc) || rpc.Code != "snapshot-stale" {
		t.Fatal(err)
	}
	d.status.Store(statusSnapshot{Updated: time.Now().Add(-time.Second)})
	page, err := d.board(nil)
	if err != nil || page.SnapshotAgeMillis < 1000 {
		t.Fatalf("%+v %v", page, err)
	}
}
func TestResumeGuard(t *testing.T) {
	for _, v := range []struct {
		after, floor int64
		ok           bool
	}{{0, 0, true}, {3, 3, true}, {4, 3, true}, {2, 3, false}} {
		if CanResumeEvents(v.after, v.floor) != v.ok {
			t.Fatal(v)
		}
	}
}

func TestCachedScheduleAndControllerAvoidWriterMutex(t *testing.T) {
	d := &Daemon{}
	d.status.Store(statusSnapshot{Updated: time.Now(), ControllerUpdated: time.Now(), Schedules: map[string]json.RawMessage{"s": json.RawMessage(`{"id":"s"}`)}, ScheduleList: json.RawMessage(`[{"id":"s"}]`), ControllerHealth: json.RawMessage(`{"consistent":true}`)})
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, request := range []api.Request{{Method: "Schedule", Params: json.RawMessage(`{"action":"list"}`)}, {Method: "Schedule", Params: json.RawMessage(`{"action":"inspect","id":"s"}`)}, {Method: "Controller", Params: json.RawMessage(`{"action":"health"}`)}} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := d.handle(ctx, request)
		cancel()
		if err != nil {
			t.Fatal(request.Method, err)
		}
	}
}

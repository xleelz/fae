package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// FAE-ROB-010: a worker start racing Close is either counted before Close's
// Wait or refused; it is never Added beside the Wait (run with -race).
func TestWorkerStartsRefusedOnceCloseBegins(t *testing.T) {
	d := &Daemon{}
	var starters sync.WaitGroup
	var started, refused atomic.Int64
	for range 64 {
		starters.Add(1)
		go func() {
			defer starters.Done()
			if d.beginWorker() != nil {
				refused.Add(1)
				return
			}
			started.Add(1)
			go func() {
				time.Sleep(time.Millisecond)
				d.workers.Done()
			}()
		}()
	}
	d.closeWorkers()
	d.workers.Wait()
	starters.Wait()
	if started.Load()+refused.Load() != 64 {
		t.Fatal(started.Load(), refused.Load())
	}
	if e := d.beginWorker(); rpcCode(e) != "shutting-down" {
		t.Fatal("worker started after Close began", e)
	}
}

// The Prophet and Land handlers reserve their worker before any write, so a
// request that passed the early shutdown refusal just before Close began is
// refused there with nothing queued, instead of racing Close's Wait.
func TestRPCWorkerStartsRefusedOnceCloseBegins(t *testing.T) {
	cfg, ctx := fixture(t)
	var turns atomic.Int64
	cfg.ProphetRunner = func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		turns.Add(1)
		return json.Marshal(ProphetAnswer{Reply: "ok", Proposals: []string{}})
	}
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, e = call(t, d, "Admit", AdmitParams{ID: "alpha", Content: "alpha", Tree: "tree", AuthorizingMessage: "message"}, "alpha"); e != nil {
		t.Fatal(e)
	}
	// Close has begun but has not yet cancelled the context: the window after
	// a handler's early refusal.
	d.closeWorkers()
	if _, e = call(t, d, "ProphetChat", ProphetParams{Task: "alpha", Text: "hello"}, ""); rpcCode(e) != "shutting-down" {
		t.Fatal("Prophet turn started after Close began", e)
	}
	if _, e = call(t, d, "Land", LandParams{Tasks: []string{"alpha"}, Target: "main", AuthorizingMessage: "message"}, ""); rpcCode(e) != "shutting-down" {
		t.Fatal("landing started after Close began", e)
	}
	if head, e := d.store.ProphetHead(ctx, "alpha"); e != nil || head != 0 {
		t.Fatal("refused Prophet turn was queued", head, e)
	}
	time.Sleep(100 * time.Millisecond)
	if turns.Load() != 0 {
		t.Fatal("refused Prophet turn ran")
	}
}

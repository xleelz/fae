package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/workflow"
)

// FAE-SCH-011 clause 4: a heavy-op waiter not admitted within 300 s emits
// exactly one line naming its wait and the current holders, and the same as
// an event on the waiting task.
func TestHeavyLeaseWaitingLineAndEventOnce(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	a := admissionFixture(t, 1)
	a.daemon = d
	var skew atomic.Int64
	a.queueClock = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	doc := capsDoc("heavy", 1, 1)
	v, e := newScheduleView(map[string]controlplane.JobSnapshot{"holder": viewJob(t, "holder", 10, doc, 1, "spec"), "waiter": viewJob(t, "waiter", 10, doc, 1, "spec")})
	if e != nil {
		t.Fatal(e)
	}
	a.view.Store(v)
	effect := func(task string) engine.Dispatch {
		return engine.Dispatch{TaskID: task, Stage: "effect", Config: workflow.Stage{Primitive: workflow.ExternalEffect, Weight: 1}}
	}
	hold, e := a.Acquire(ctx, effect("holder"), execution.Binding{}, engine.Session{ID: "holder-effect"})
	if e != nil {
		t.Fatal(e)
	}
	waited := make(chan error, 1)
	go func() {
		release, e := a.Acquire(ctx, effect("waiter"), execution.Binding{}, engine.Session{ID: "waiter-effect"})
		if e == nil {
			e = release()
		}
		waited <- e
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if p, e := a.host.Snapshot(ctx); e == nil && len(p.Waiting) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter not queued")
		}
	}
	skew.Store(int64(301 * time.Second))
	var event controlplane.Event
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if event, e = d.store.LastJobEvent(ctx, "waiter", heavyWaitingEventKind); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no waiting event", e)
		}
	}
	var payload struct {
		Wait    float64  `json:"wait_seconds"`
		Holders []string `json:"holders"`
	}
	if e = json.Unmarshal(event.Payload, &payload); e != nil || payload.Wait < 300 || len(payload.Holders) != 1 || payload.Holders[0] != a.proofID("holder-effect") {
		t.Fatalf("waiting event %s %v", event.Payload, e)
	}
	time.Sleep(500 * time.Millisecond) // further advances must stay silent
	events, e := d.store.JobEvents(ctx, "waiter", 0, 100)
	n := 0
	for _, ev := range events {
		if ev.Kind == heavyWaitingEventKind {
			n++
		}
	}
	raw, _ := os.ReadFile(d.layout.Log)
	if lines := strings.Count(string(raw), "host lease waiting reason=HeavyLease id="+a.proofID("waiter-effect")); e != nil || n != 1 || lines != 1 {
		t.Fatalf("waiting reported %d events and %d lines: %v", n, lines, e)
	}
	if e = hold(); e != nil {
		t.Fatal(e)
	}
	if e = <-waited; e != nil {
		t.Fatal(e)
	}
}

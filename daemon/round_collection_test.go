package daemon

import (
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

// FAE-SCH-009 acceptance: three tasks whose turns finish together are all
// collected and advanced in the same round, before that round dispatches; a
// ready wake never runs a round past a delivered completion.
func TestDriveRoundCollectsEveryCompletionBeforeDispatch(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.MaxInFlight = 0
	gated := &drainExecutor{started: make(chan string, 16), release: make(chan struct{}), delegate: engine.NewScripted(defaults.HappyScript())}
	cfg.Executor = gated
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(t, d) })
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	ids := []string{"same-round-a", "same-round-b", "same-round-c"}
	for _, id := range ids {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	starts := func(n int) map[string]bool {
		t.Helper()
		got := map[string]bool{}
		deadline := time.After(20 * time.Second)
		for len(got) < n {
			select {
			case id := <-gated.started:
				got[id] = true
			case <-deadline:
				t.Fatalf("only %d of %d turns started: %v", len(got), n, got)
			}
		}
		return got
	}
	// Admission enters each task's first stage; a later round launches it.
	for i := 0; i < 10 && len(gated.started) < 3; i++ {
		if err = d.round(); err != nil {
			t.Fatal(err)
		}
	}
	starts(3)
	sequence := func(id string) int {
		d.mu.Lock()
		defer d.mu.Unlock()
		var e envelope
		if err := decodeEnvelope(d.jobs[id].Payload, &e); err != nil {
			t.Fatal(err)
		}
		return len(e.Job.History)
	}
	before := map[string]int{}
	for _, id := range ids {
		before[id] = sequence(id)
		gated.release <- struct{}{}
	}
	for deadline := time.Now().Add(20 * time.Second); len(d.results) < 3; {
		if time.Now().After(deadline) {
			t.Fatal("completions not delivered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = d.round(); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if sequence(id) <= before[id] {
			t.Fatalf("%s not collected in the round", id)
		}
	}
	// The collected tasks go on to dispatch their next turns together.
	for i := 0; i < 10 && len(gated.started) < 3; i++ {
		if err = d.round(); err != nil {
			t.Fatal(err)
		}
	}
	starts(3)
}

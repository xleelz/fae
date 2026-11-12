package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A control waits for an in-progress run instead of being refused, and no new
// run starts while it waits, so even back-to-back runs can be paused.
func TestScheduleControlWaitsForRunAndRunsYield(t *testing.T) {
	var mu sync.Mutex
	var controls atomic.Int32
	if !scheduleRunLock(&mu, &controls) {
		t.Fatal("idle schedule refused a run")
	}
	acquired := make(chan bool, 1)
	go func() { acquired <- scheduleControlLock(context.Background(), &mu, &controls) }()
	deadline := time.Now().Add(time.Second)
	for controls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("control never registered")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Unlock() // the in-progress run finishes
	if !<-acquired {
		t.Fatal("control refused after the run finished")
	}
	if scheduleRunLock(&mu, &controls) {
		t.Fatal("run started while the control held the schedule")
	}
	controls.Add(1) // another control waiting
	mu.Unlock()
	if scheduleRunLock(&mu, &controls) {
		t.Fatal("run started while a control waited")
	}
	controls.Add(-1)
	if !scheduleRunLock(&mu, &controls) {
		t.Fatal("run refused once no control waited")
	}
	mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	mu.Lock()
	cancel()
	if scheduleControlLock(ctx, &mu, &controls) || controls.Load() != 0 {
		t.Fatal("cancelled control acquired or left a waiter")
	}
	mu.Unlock()
}

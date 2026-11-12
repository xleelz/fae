package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// scheduleControlWait bounds how long an operator control waits for an
// in-progress schedule run, inside the 5 s RPC deadline.
const scheduleControlWait = 4 * time.Second

// scheduleRunLock is a schedule tick's non-blocking acquire. It yields to a
// waiting operator control, so a short-interval schedule whose runs follow
// each other back to back can still be paused or reconfigured.
func scheduleRunLock(mu *sync.Mutex, controls *atomic.Int32) bool {
	return controls.Load() == 0 && mu.TryLock()
}

// scheduleControlLock acquires a schedule for an operator control. While it
// waits no new run starts; an in-progress run is waited for (bounded) rather
// than refusing the control at once.
func scheduleControlLock(ctx context.Context, mu *sync.Mutex, controls *atomic.Int32) bool {
	controls.Add(1)
	defer controls.Add(-1)
	deadline := time.Now().Add(scheduleControlWait)
	for !mu.TryLock() {
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

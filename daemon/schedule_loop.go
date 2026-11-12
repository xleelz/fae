package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/xleelz/fae/controlplane"
)

// FAE-ARC-002: background schedules are deadline-driven. A worker reads its
// durable record when it is due or when the record was written (a wake), and
// otherwise blocks; an idle daemon makes no plane round trip for them. A due
// run that is held (drain, maintenance, a concurrent RPC, a plane error) is
// retried with backoff up to scheduleRetryMax.
const (
	scheduleRetryMin = time.Second
	scheduleRetryMax = 30 * time.Second
	// eventWindowMinGap bounds event-window runs during activity to the
	// former one-second cadence; an idle plane is not re-read at all.
	eventWindowMinGap = time.Second
)

// scheduleWakes are coalescing, lazily created wakes for the schedule workers.
type scheduleWakes struct {
	once                           sync.Once
	backup, replication, retention chan struct{}
}

func (w *scheduleWakes) init() {
	w.once.Do(func() {
		w.backup, w.replication, w.retention = make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1)
	})
}
func poke(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}
func (d *Daemon) wakeBackupSchedule()      { d.scheduleWakes.init(); poke(d.scheduleWakes.backup) }
func (d *Daemon) wakeReplicationSchedule() { d.scheduleWakes.init(); poke(d.scheduleWakes.replication) }
func (d *Daemon) wakeRetentionSchedule()   { d.scheduleWakes.init(); poke(d.scheduleWakes.retention) }

// scheduleDue is a schedule record's next run: due reports a configured,
// active, idle schedule; any other state waits for a write to the record.
type scheduleDue func(context.Context) (next time.Time, due bool, err error)

// nextScheduleWait is the sleep after a tick: until the due time, or a backoff
// retry when the run is still due (held) or the record was unreadable; zero
// (block until a wake) when nothing is scheduled.
func nextScheduleWait(now, next time.Time, due bool, err error, retry time.Duration) (wait, nextRetry time.Duration) {
	backoff := min(retry*2, scheduleRetryMax)
	if err != nil {
		return retry, backoff
	}
	if !due {
		return 0, scheduleRetryMin
	}
	if until := next.Sub(now); until > 0 {
		return until, scheduleRetryMin
	}
	return retry, backoff
}

func (d *Daemon) runSchedule(wake chan struct{}, tick func(context.Context, time.Time) error, read scheduleDue) {
	defer d.workers.Done()
	retry := scheduleRetryMin
	for {
		// A wake only re-reads the record; the tick (and its run lock) is
		// taken only once the schedule is due by the wall clock.
		ctx, cancel := context.WithTimeout(d.ctx, 2*time.Minute)
		next, due, err := read(ctx)
		if now := time.Now(); err == nil && due && !now.Before(next) {
			_ = tick(ctx, now)
			next, due, err = read(ctx)
		}
		cancel()
		var wait time.Duration
		wait, retry = nextScheduleWait(time.Now(), next, due, err, retry)
		var timer <-chan time.Time
		var t *time.Timer
		if wait > 0 {
			t = time.NewTimer(wait)
			timer = t.C
		}
		select {
		case <-d.ctx.Done():
			return
		case <-wake:
		case <-timer:
		}
		if t != nil {
			t.Stop()
		}
	}
}

func (d *Daemon) maintainBackupSchedule() {
	d.scheduleWakes.init()
	d.runSchedule(d.scheduleWakes.backup, d.tickBackupSchedule, func(ctx context.Context) (time.Time, bool, error) {
		s, err := d.readBackupSchedule(ctx)
		return s.NextAt, err == nil && s.State == "active" && !s.Running, err
	})
}
func (d *Daemon) maintainReplicationSchedule() {
	d.scheduleWakes.init()
	d.runSchedule(d.scheduleWakes.replication, d.tickReplicationSchedule, func(ctx context.Context) (time.Time, bool, error) {
		s, err := d.readReplicationSchedule(ctx)
		return s.NextAt, err == nil && s.State == "active" && !s.Running, err
	})
}
func (d *Daemon) maintainRetentionSchedule() {
	d.scheduleWakes.init()
	d.runSchedule(d.scheduleWakes.retention, d.tickRetentionSchedule, func(ctx context.Context) (time.Time, bool, error) {
		s, err := d.readRetentionSchedule(ctx)
		return s.NextAt, err == nil && s.State == "active" && !s.Running, err
	})
}

// maintainEventWindow advances the retention floor when events are written
// (at most once per eventWindowMinGap) and when the oldest retained event
// ages out of the window; between those it blocks.
func (d *Daemon) maintainEventWindow(store *controlplane.Store) {
	defer d.workers.Done()
	age, count := d.config.EventRetentionAge, d.config.EventRetentionCount
	if age == 0 {
		age = 24 * time.Hour
	}
	if count == 0 {
		count = 100000
	}
	retry := scheduleRetryMin
	for {
		d.eventMu.Lock()
		written := d.eventWake
		d.eventMu.Unlock()
		started := time.Now()
		ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
		floor, err := store.EventWindow(ctx, age, count, started.UTC())
		var oldest []controlplane.Event
		if err == nil {
			oldest, err = store.Events(ctx, floor, 1)
		}
		cancel()
		var timer <-chan time.Time
		switch {
		case err != nil:
			timer, retry = time.After(retry), min(retry*2, scheduleRetryMax)
		case len(oldest) > 0:
			retry = scheduleRetryMin
			timer = time.After(max(time.Until(oldest[0].Timestamp.Add(age)), eventWindowMinGap))
		default:
			retry = scheduleRetryMin
		}
		select {
		case <-d.ctx.Done():
			return
		case <-timer:
			continue
		case <-written:
		}
		select { // coalesce a burst of writes
		case <-d.ctx.Done():
			return
		case <-time.After(eventWindowMinGap - time.Since(started)):
		}
	}
}

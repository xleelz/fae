package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/xleelz/fae/controlplane"
)

// planeOutage is a detected plane failure awaiting recovery (FAE-CP-006).
type planeOutage struct {
	Cause    string    `json:"cause"`
	Detected time.Time `json:"detected_at"`
	Detail   string    `json:"detail,omitempty"`
	PID      int       `json:"pid"`
}

// planeOutageRecord is the durable record of one outage and its recovery.
// It is written as soon as the plane answers again, with Checked false, and
// again with the consistency check's result once the check has run.
type planeOutageRecord struct {
	planeOutage
	Recovered   time.Time                `json:"recovered_at"`
	OutageMS    int64                    `json:"outage_ms"`
	Attempts    int                      `json:"attempts"`
	NewPID      int                      `json:"new_pid"`
	Checked     bool                     `json:"checked"`
	Consistency controlplane.Consistency `json:"consistency"`
}

const (
	// planeProbePeriod and planeProbeDeadline bound hung-plane detection:
	// two failed pings a period apart while the daemon is not idle.
	planeProbePeriod   = 5 * time.Second
	planeProbeDeadline = 5 * time.Second
)

func (d *Daemon) currentChild() *controlplane.Server {
	d.childMu.RLock()
	defer d.childMu.RUnlock()
	return d.child
}

// monitorPlane detects a hung Dolt whose child process is still alive: while
// the daemon is not idle it pings the plane under a deadline, and two
// consecutive failures kill the child. A dead child the drive has not
// restarted within a probe period (a drive stopped by the outage, or blocked
// elsewhere) is restarted here, and a drive the outage stopped is re-armed.
// An idle daemon sends no probe (FAE-ARC-002).
func (d *Daemon) monitorPlane() {
	defer d.workers.Done()
	ticker := time.NewTicker(planeProbePeriod)
	defer ticker.Stop()
	failures := 0
	var exitedAt time.Time
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		}
		child := d.currentChild()
		if child == nil {
			continue
		}
		select {
		case <-child.Done():
			failures = 0
			if exitedAt.IsZero() {
				exitedAt = time.Now()
				continue
			}
			if time.Since(exitedAt) < planeProbePeriod {
				continue
			}
			exitedAt = time.Time{}
			if err := d.restartPlane(); err != nil {
				fmt.Fprintf(componentLog(d.log, "plane"), "Dolt restart failed: %v\n", err)
				continue
			}
			d.rearmAfterRecovery()
			continue
		default:
			exitedAt = time.Time{}
		}
		if d.driveIdle(time.Now()) {
			failures = 0
			continue
		}
		ctx, cancel := context.WithTimeout(d.ctx, planeProbeDeadline)
		err := d.store.Ping(ctx)
		cancel()
		if err == nil || d.ctx.Err() != nil {
			failures = 0
			continue
		}
		if failures++; failures < 2 {
			continue
		}
		failures = 0
		d.outage.CompareAndSwap(nil, &planeOutage{Cause: "unresponsive", Detected: time.Now().UTC(), Detail: err.Error(), PID: child.PID()})
		fmt.Fprintf(componentLog(d.log, "plane"), "Dolt pid %d unresponsive: %v; killing it for a bounded restart\n", child.PID(), err)
		_ = child.Kill()
	}
}

// rearmAfterRecovery re-arms a drive generation the outage ended; a drive an
// operator stopped stays stopped.
func (d *Daemon) rearmAfterRecovery() {
	d.mu.Lock()
	defer d.mu.Unlock()
	cause, _ := d.driveCause.Load().(string)
	if d.driveAlive.Load() || d.drivePending.Load() || d.ctx.Err() != nil || cause != "drive-failure" {
		return
	}
	if err := d.requestDriveRearm(d.ctx, controlplane.Audit{Actor: "daemon", Action: "drive-rearm", Outcome: "ok"}); err != nil {
		fmt.Fprintf(componentLog(d.log, "plane"), "drive rearm after plane recovery refused: %v\n", err)
	}
}

// restartPlane relaunches the recorded child with the same private config,
// with bounded, backed-off attempts (default 3). SQL connections reconnect
// through database/sql; durable snapshots remain the authority. Once the
// plane answers it re-validates the schema and the plane's own consistency
// and records the outage and the recovery; the drive then re-validates its
// cached checkpoints against the plane before acting again (FAE-CP-006).
func (d *Daemon) restartPlane() error {
	// One restart at a time: a caller that finds a live child after another
	// restarted it has nothing to do.
	d.restartMu.Lock()
	defer d.restartMu.Unlock()
	if c := d.currentChild(); c != nil {
		select {
		case <-c.Done():
		default:
			return nil
		}
	}
	binary := d.config.DoltBinary
	if binary == "" {
		binary = "dolt"
	}
	old := d.currentChild()
	outage := planeOutage{Cause: "exited", Detected: time.Now().UTC()}
	if detected := d.outage.Swap(nil); detected != nil {
		outage = *detected
	} else if old != nil {
		outage.PID = old.PID()
		if err := old.Err(); err != nil {
			outage.Detail = err.Error()
		}
	}
	var last error
	for attempt := 0; attempt < d.doltRestartAttempts(); attempt++ {
		select {
		case <-d.ctx.Done():
			return d.ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
		port, err := controlplane.Port(d.layout.Data, d.config.Port)
		if err != nil {
			return err
		}
		child, err := controlplane.StartServer(d.childCtx, controlplane.ServerConfig{Binary: binary, DataDir: d.layout.Data, Port: port, Log: componentStream(d.log, "dolt"), StopWait: d.doltStopWait(), ConfigFile: filepath.Join(d.layout.Run, "dolt.yaml"), Env: scopedEnv(d.layout.Root), OwnerRegistry: filepath.Join(d.layout.Run, "plane-processes")})
		if err != nil {
			last = err
			continue
		}
		d.childMu.Lock()
		d.child = child
		d.childMu.Unlock()
		for probe := 0; probe < 100; probe++ {
			ctx, cancel := context.WithTimeout(d.ctx, time.Second)
			err = d.store.Migrate(ctx)
			cancel()
			if err == nil {
				d.lifecycle("dolt-crashed", map[string]any{"detected": "plane unreachable", "detected_at": outage.Detected})
				if err := d.recordRecovery(outage, attempt+1, child.PID()); err != nil {
					return err
				}
				d.lifecycle("dolt-restarted", map[string]any{"attempt": attempt + 1, "dolt_pid": child.PID(), "detected_at": outage.Detected, "recovered_at": time.Now().UTC()})
				return nil
			}
			select {
			case <-d.ctx.Done():
				return d.ctx.Err()
			case <-child.Done():
				probe = 100
			case <-time.After(50 * time.Millisecond):
			}
		}
		_ = child.Stop()
		last = err
	}
	return fmt.Errorf("Dolt restart exhausted after %d attempts: %w", d.doltRestartAttempts(), last)
}

// recordRecovery records the outage durably and checks the restarted plane.
// The outage and the recovery are recorded first, then the check runs with
// the start path's budget (startupReadBound per read, no overall bound, so a
// plane with many diverged tasks is checked whole while a hung read fails),
// and the record is completed with its result. Whatever the check's outcome,
// the drive compares its cached checkpoints with the plane before acting
// again (revalidateCheckpoints, which repeats the check): a check that fails
// here is not skipped, and a plane whose own invariants fail is not resumed
// on.
func (d *Daemon) recordRecovery(outage planeOutage, attempts, pid int) error {
	now := time.Now().UTC()
	record := planeOutageRecord{planeOutage: outage, Recovered: now, OutageMS: now.Sub(outage.Detected).Milliseconds(), Attempts: attempts, NewPID: pid}
	put := func(outcome string) error {
		raw, _ := json.Marshal(record)
		ctx, cancel := context.WithTimeout(d.ctx, startupReadBound)
		defer cancel()
		return d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "Dolt recovered", Inputs: raw, Outcome: outcome}, "plane-recovery", raw)
	}
	if err := put("unchecked"); err != nil {
		d.revalidate.Store(true)
		return fmt.Errorf("record plane recovery: %w", err)
	}
	d.revalidate.Store(true)
	consistency, err := d.store.CheckConsistencyBounded(d.ctx, startupReadBound)
	if err != nil {
		fmt.Fprintf(componentLog(d.log, "plane"), "Dolt recovered: cause=%s outage=%dms attempts=%d pid=%d; consistency check failed, the drive re-validates before acting: %v\n", outage.Cause, record.OutageMS, attempts, pid, err)
		return fmt.Errorf("re-validate restarted plane: %w", err)
	}
	record.Checked, record.Consistency = consistency.Consistent(), consistency
	outcome := "ok"
	if !consistency.Consistent() {
		outcome = "inconsistent"
	}
	if err = put(outcome); err != nil {
		return err
	}
	fmt.Fprintf(componentLog(d.log, "plane"), "Dolt recovered: cause=%s outage=%dms attempts=%d pid=%d consistent=%t\n", outage.Cause, record.OutageMS, attempts, pid, consistency.Consistent())
	if consistency.EventClock < consistency.LastEvent {
		return errors.New("restarted plane's event clock is behind its events")
	}
	return nil
}

// revalidateCheckpoints runs in the drive after a plane restart: every cached
// checkpoint is compared with the plane's row. A newer durable version (a
// write whose acknowledgement the outage lost) is re-read and re-validated, a
// durable active job missing from the cache is loaded, and a task whose
// relational rows diverged from its snapshot is quarantined: nothing is
// guessed or replayed.
func (d *Daemon) revalidateCheckpoints(ctx context.Context) error {
	if !d.revalidate.Swap(false) {
		return nil
	}
	versions, err := d.store.ActiveJobVersions(ctx)
	if err != nil {
		d.revalidate.Store(true)
		return err
	}
	consistency, err := d.store.CheckConsistencyBounded(ctx, startupReadBound)
	if err != nil {
		d.revalidate.Store(true)
		return err
	}
	if consistency.EventClock < consistency.LastEvent {
		d.revalidate.Store(true)
		return fmt.Errorf("restarted plane's event clock (%d) is behind its last event (%d)", consistency.EventClock, consistency.LastEvent)
	}
	if err := d.completeRecoveryRecord(ctx, consistency); err != nil {
		d.revalidate.Store(true)
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range consistency.Diverged {
		d.quarantine(ctx, id, taskErr("plane-recovery", errors.New("task rows diverged from the durable snapshot")))
	}
	pending := ""
	if d.pendingCheckpoint != nil {
		pending = d.pendingCheckpoint.Before.ID
	}
	stale := []string{}
	for id, version := range versions {
		cached, ok := d.jobs[id]
		if id != pending && d.quarantined[id] == "" && !d.quarantineBook.retired[id] && (!ok || cached.Version != version) {
			stale = append(stale, id)
		}
	}
	// A cached active task the plane no longer holds as active finished in a
	// checkpoint whose acknowledgement was lost.
	for id, cached := range d.jobs {
		if _, active := versions[id]; !active && id != pending && !backlogTerminal(cached.State) {
			stale = append(stale, id)
		}
	}
	for _, id := range stale {
		fresh, err := d.store.GetJob(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			d.revalidate.Store(true)
			return err
		}
		if _, err = d.validateStoredJob(ctx, fresh); err != nil {
			d.quarantine(ctx, id, err)
			continue
		}
		d.jobs[id] = fresh
	}
	d.publishStatus()
	return nil
}

func (d *Daemon) completeRecoveryRecord(ctx context.Context, consistency controlplane.Consistency) error {
	if !consistency.Consistent() {
		return nil
	}
	readCtx, cancel := context.WithTimeout(ctx, startupReadBound)
	raw, err := d.store.GetMarker(readCtx, "plane-recovery")
	cancel()
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var record planeOutageRecord
	if err := json.Unmarshal(raw, &record); err != nil || record.Checked {
		return err
	}
	record.Checked, record.Consistency = true, consistency
	updated, err := json.Marshal(record)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, startupReadBound)
	defer cancel()
	return d.store.PutMarker(writeCtx, controlplane.Audit{Actor: "daemon", Action: "Dolt recovered", Inputs: updated, Outcome: "ok"}, "plane-recovery", updated)
}

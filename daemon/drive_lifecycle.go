package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/kernel"
)

// errRearmBudget is a spent re-arm budget (Appendix A: 3 per cause per hour).
var errRearmBudget = errors.New("drive rearm budget exhausted")

type driveBudget struct {
	Start time.Time
	Count int
}

// requestDriveRearm runs under d.mu. The plane owns the budget, so replacing a
// watchdog or daemon cannot reset it. Operator-supplied cause strings are ignored.
func (d *Daemon) requestDriveRearm(ctx context.Context, audit controlplane.Audit) error {
	if d.ctx.Err() != nil {
		return errors.New("daemon is shutting down")
	}
	if d.driveAlive.Load() || d.drivePending.Load() {
		return nil
	}
	cause, _ := d.driveCause.Load().(string)
	if cause == "" {
		cause = "drive-failure"
	}
	var budget driveBudget
	raw, err := d.store.GetMarker(ctx, "drive-rearm/"+cause)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if err = json.Unmarshal(raw, &budget); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	limit, window := d.rearmBounds()
	if budget.Start.IsZero() || now.Sub(budget.Start) >= window {
		budget = driveBudget{Start: now}
	}
	if !kernel.CanRearmDrive(d.driveAlive.Load(), d.drivePending.Load(), budget.Count < limit, d.ctx.Err() == nil) {
		return fmt.Errorf("%w for cause %s", errRearmBudget, cause)
	}
	if err = d.reconcileLoopCheckpoint(ctx); err != nil {
		return err
	}
	settled, err := d.settleCheckpoint(ctx)
	if err != nil {
		return err
	}
	if !settled {
		return errors.New("pending drive checkpoint is not yet verifiable")
	}
	// Check durable and cached bindings before scheduling another generation.
	// A task whose row is invalid or diverged is isolated, never a reason to
	// keep the drive down for every other task (FAE-ROB-001).
	for id, snapshot := range d.jobs {
		var e envelope
		if err = decodeEnvelope(snapshot.Payload, &e); err == nil {
			err = engine.Validate(e.Job)
		}
		if err != nil {
			d.quarantine(ctx, id, taskErr("rearm", err))
			continue
		}
		actual, readErr := d.store.GetJob(ctx, id)
		if readErr != nil {
			return readErr
		}
		want, _ := controlplane.CanonicalJSON(snapshot.Payload)
		have, _ := controlplane.CanonicalJSON(actual.Payload)
		if actual.Version != snapshot.Version || !bytes.Equal(have, want) {
			if err = d.taskFault(ctx, id, fmt.Errorf("drive checkpoint diverged for %s: %w", id, controlplane.ErrConflict)); err != nil {
				return err
			}
		}
	}
	budget.Count++
	raw, _ = json.Marshal(budget)
	audit.Action = "drive-rearm-" + cause
	if err = d.store.PutMarker(ctx, audit, "drive-rearm/"+cause, raw); err != nil {
		return err
	}
	d.drivePending.Store(true)
	d.driveRearm <- struct{}{}
	d.notifyEvents()
	return nil
}

// rearmBounds is the configured re-arm budget per cause and its window.
func (d *Daemon) rearmBounds() (int, time.Duration) {
	limit, window := d.config.RearmBudget, d.config.RearmWindow
	if limit <= 0 {
		limit = 3
	}
	if window <= 0 {
		window = time.Hour
	}
	return limit, window
}

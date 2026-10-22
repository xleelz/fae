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
	if budget.Start.IsZero() || now.Sub(budget.Start) >= time.Hour {
		budget = driveBudget{Start: now}
	}
	if !kernel.CanRearmDrive(d.driveAlive.Load(), d.drivePending.Load(), budget.Count < 3, d.ctx.Err() == nil) {
		return errors.New("drive rearm budget exhausted for cause " + cause)
	}
	if err = d.reconcileLoopCheckpoint(ctx); err != nil {
		return err
	}
	if err = d.reconcileJobCheckpoint(ctx); err != nil {
		return err
	}
	// Check durable and cached bindings before scheduling another generation.
	for id, snapshot := range d.jobs {
		var e envelope
		if err = decodeEnvelope(snapshot.Payload, &e); err != nil {
			return err
		}
		if err = engine.Validate(e.Job); err != nil {
			return err
		}
		actual, readErr := d.store.GetJob(ctx, id)
		if readErr != nil {
			return readErr
		}
		want, _ := controlplane.CanonicalJSON(snapshot.Payload)
		have, _ := controlplane.CanonicalJSON(actual.Payload)
		if actual.Version != snapshot.Version || !bytes.Equal(have, want) {
			return fmt.Errorf("drive checkpoint diverged for %s", id)
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

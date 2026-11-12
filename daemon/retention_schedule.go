package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
)

const retentionScheduleKey = "backup-retention-schedule"

type RetentionScheduleParams struct {
	Action             string `json:"action"`
	IntervalSeconds    int64  `json:"interval_seconds,omitempty"`
	Keep               int    `json:"keep,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}
type RetentionScheduleRecord struct {
	State              string    `json:"state"`
	IntervalSeconds    int64     `json:"interval_seconds"`
	Keep               int       `json:"keep"`
	AuthorizingMessage string    `json:"authorizing_message"`
	NextAt             time.Time `json:"next_at"`
	Running            bool      `json:"running"`
	RunCount           int64     `json:"run_count"`
	LastStarted        time.Time `json:"last_started,omitempty"`
	LastFinished       time.Time `json:"last_finished,omitempty"`
	LastError          string    `json:"last_error,omitempty"`
	LastRemoved        int       `json:"last_removed"`
}

func (d *Daemon) readRetentionSchedule(ctx context.Context) (RetentionScheduleRecord, error) {
	var s RetentionScheduleRecord
	raw, e := d.store.GetMarker(ctx, retentionScheduleKey)
	if errors.Is(e, sql.ErrNoRows) {
		s.State = "unconfigured"
		return s, nil
	}
	if e != nil {
		return s, e
	}
	if e = decode(raw, &s); e != nil {
		return s, e
	}
	if (s.State != "active" && s.State != "paused" && s.State != "blocked") || s.IntervalSeconds < 1 || s.IntervalSeconds > 31536000 || s.Keep < 1 || s.Keep > 10000 || s.AuthorizingMessage == "" || s.NextAt.IsZero() || s.RunCount < 0 {
		return s, errors.New("invalid durable retention schedule")
	}
	return s, nil
}
func (d *Daemon) saveRetentionSchedule(ctx context.Context, s RetentionScheduleRecord, a controlplane.Audit) error {
	raw, e := json.Marshal(s)
	if e != nil {
		return e
	}
	defer d.wakeRetentionSchedule()
	return d.store.PutMarker(ctx, a, retentionScheduleKey, raw)
}
func (d *Daemon) retentionRecurrenceAuthority(ctx context.Context, message string) error {
	if e := d.store.AuthorizeReadOnlyEffect(ctx, message, "operator", "BackupRetention", "runtime", actionClasses()); e != nil {
		return e
	}
	raw, e := d.store.MessageText(ctx, message)
	if e != nil {
		return e
	}
	var directed controlplane.DirectedMessage
	if e = json.Unmarshal([]byte(raw), &directed); e != nil {
		return errors.New("automatic retention requires explicit structured runtime or BackupRetention grant")
	}
	for _, v := range directed.Authorize.Classes {
		if v == "runtime" {
			return nil
		}
	}
	for _, v := range directed.Authorize.Verbs {
		if v == "BackupRetention" {
			return nil
		}
	}
	return errors.New("automatic retention requires explicit runtime or BackupRetention grant")
}
func (d *Daemon) backupRetentionScheduleRPC(ctx context.Context, r api.Request) (any, error) {
	principal, ok := api.AuthenticatedPrincipal(ctx)
	if !ok || principal.Actor != "operator" || principal.Task != "" {
		return nil, errors.New("retention schedule requires instance operator")
	}
	var p RetentionScheduleParams
	if e := decode(r.Params, &p); e != nil {
		return nil, e
	}
	if p.Action == "status" {
		return d.readRetentionSchedule(ctx)
	}
	if p.Action != "configure" && p.Action != "pause" && p.Action != "resume" {
		return nil, errors.New("unknown retention schedule action")
	}
	if p.Action != "configure" && (p.IntervalSeconds != 0 || p.Keep != 0) {
		return nil, errors.New("only configure accepts interval and keep")
	}
	if e := d.store.AuthorizeReadOnlyEffect(ctx, p.AuthorizingMessage, "operator", "BackupRetentionSchedule", "runtime", actionClasses()); e != nil {
		return nil, e
	}
	if p.Action != "pause" {
		if e := d.retentionRecurrenceAuthority(ctx, p.AuthorizingMessage); e != nil {
			return nil, e
		}
	}
	authorized, e := d.store.AuthorizeAction(ctx, p.AuthorizingMessage, "operator", "BackupRetentionSchedule", "runtime", false, actionClasses())
	if e != nil {
		return nil, e
	}
	if !scheduleControlLock(ctx, &d.retentionScheduleMu, &d.retentionControls) {
		return nil, errors.New("retention schedule running; retry control after current run")
	}
	defer d.retentionScheduleMu.Unlock()
	s, e := d.readRetentionSchedule(ctx)
	if e != nil {
		return nil, e
	}
	switch p.Action {
	case "configure":
		if p.IntervalSeconds < 1 || p.IntervalSeconds > 31536000 || p.Keep < 1 || p.Keep > 10000 {
			return nil, errors.New("interval_seconds must be 1..31536000 and keep 1..10000")
		}
		s = RetentionScheduleRecord{State: "active", IntervalSeconds: p.IntervalSeconds, Keep: p.Keep, AuthorizingMessage: p.AuthorizingMessage, NextAt: time.Now().UTC().Add(time.Duration(p.IntervalSeconds) * time.Second)}
	case "pause":
		if s.State == "unconfigured" {
			return nil, errors.New("retention schedule not configured")
		}
		s.State = "paused"
		s.Running = false
	case "resume":
		if s.State != "paused" && s.State != "blocked" {
			return nil, errors.New("only paused or blocked retention schedule can resume")
		}
		s.State = "active"
		s.Running = false
		s.LastError = ""
		s.AuthorizingMessage = p.AuthorizingMessage
		s.NextAt = time.Now().UTC().Add(time.Duration(s.IntervalSeconds) * time.Second)
	}
	e = d.saveRetentionSchedule(authorized, s, controlplane.Audit{Actor: "operator", Action: "BackupRetentionSchedule", MessageID: p.AuthorizingMessage, Inputs: r.Params, Outcome: p.Action})
	return s, e
}

// Startup never replays a stale destructive plan after an interrupted attempt.
func (d *Daemon) recoverRetentionSchedule(ctx context.Context) error {
	s, e := d.readRetentionSchedule(ctx)
	if e != nil {
		return e
	}
	if !s.Running {
		return nil
	}
	s.State = "blocked"
	s.Running = false
	s.LastError = "interrupted retention attempt; inspect backups and explicitly resume"
	return d.saveRetentionSchedule(ctx, s, controlplane.Audit{Actor: "daemon", Action: "retention schedule recovery", MessageID: s.AuthorizingMessage, Outcome: "blocked"})
}
func (d *Daemon) tickRetentionSchedule(ctx context.Context, now time.Time) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	d.mu.Lock()
	draining := d.draining
	held, heldErr := d.maintenanceHeld()
	d.mu.Unlock()
	if draining || held {
		return heldErr
	}

	if !scheduleRunLock(&d.retentionScheduleMu, &d.retentionControls) {
		return nil
	}
	defer d.retentionScheduleMu.Unlock()
	s, e := d.readRetentionSchedule(ctx)
	if e != nil {
		return e
	}
	if s.State != "active" || s.Running || now.Before(s.NextAt) {
		return nil
	}
	authErr := d.retentionRecurrenceAuthority(ctx, s.AuthorizingMessage)
	if !CanRunRetentionSchedule(s.State == "active", !now.Before(s.NextAt), authErr == nil) {
		s.State = "blocked"
		s.LastError = fmt.Sprint(authErr)
		return d.saveRetentionSchedule(ctx, s, controlplane.Audit{Actor: "daemon", Action: "retention schedule authority", MessageID: s.AuthorizingMessage, Outcome: "blocked"})
	}
	unclaimed := s
	s.Running = true
	s.RunCount++
	s.LastStarted = now.UTC()
	s.LastError = ""
	s.LastRemoved = 0
	s.NextAt = now.UTC().Add(time.Duration(s.IntervalSeconds) * time.Second)
	audit := controlplane.Audit{Actor: "daemon", Action: "retention scheduled run", MessageID: s.AuthorizingMessage, Outcome: "claimed"}
	if e = d.saveRetentionSchedule(ctx, s, audit); e != nil {
		return e
	}
	var runErr error
	if d.live != nil && d.live.guard != nil {
		sid := fmt.Sprintf("retention-schedule-%d-%d", s.LastStarted.UnixNano(), s.RunCount)
		release, e := d.live.guard.AcquireHostIO(ctx, sid)
		if e != nil {
			// Host admission deferred the run before any write (FAE-HOST-002,
			// -010). Like a park it consumes no run: the claim is withdrawn and
			// a later tick retries, as the replication schedule does.
			unclaimed.LastError = e.Error()
			audit.Outcome = "deferred"
			if saveErr := d.saveRetentionSchedule(ctx, unclaimed, audit); saveErr != nil {
				return saveErr
			}
			return e
		}
		defer release()
	}
	var preview BackupRetentionResult
	if runErr == nil {
		preview, runErr = d.backupRetention(ctx, BackupRetentionParams{Keep: s.Keep})
	}
	if runErr == nil {
		runErr = d.retentionRecurrenceAuthority(ctx, s.AuthorizingMessage)
		if runErr != nil {
			s.State = "blocked"
		}
	}
	if runErr == nil {
		var applied BackupRetentionResult
		applied, runErr = d.backupRetention(ctx, BackupRetentionParams{Keep: s.Keep, Apply: true, PlanDigest: preview.PlanDigest})
		s.LastRemoved = len(applied.Removed)
	}
	s.Running = false
	s.LastFinished = time.Now().UTC()
	// The interval runs from completion as well: runs never follow each other
	// back to back, so controls and status see the schedule idle between runs.
	if next := s.LastFinished.Add(time.Duration(s.IntervalSeconds) * time.Second); next.After(s.NextAt) {
		s.NextAt = next
	}
	audit.Outcome = "completed"
	if runErr != nil {
		s.LastError = runErr.Error()
		audit.Outcome = "failed"
	}
	// Cancellation can leave the durable claim running. Recovery blocks it.
	if e = d.saveRetentionSchedule(ctx, s, audit); e != nil {
		return e
	}
	return runErr
}

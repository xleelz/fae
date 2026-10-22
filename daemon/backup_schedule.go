package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
)

const backupScheduleKey = "backup-creation-schedule"

type backupScheduleAuthorityKey struct{}

type BackupScheduleParams struct {
	Action             string `json:"action"`
	IntervalSeconds    int64  `json:"interval_seconds,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}
type BackupScheduleRecord struct {
	State              string    `json:"state"`
	IntervalSeconds    int64     `json:"interval_seconds"`
	AuthorizingMessage string    `json:"authorizing_message"`
	NextAt             time.Time `json:"next_at"`
	Running            bool      `json:"running"`
	RunCount           int64     `json:"run_count"`
	LastStarted        time.Time `json:"last_started,omitempty"`
	LastFinished       time.Time `json:"last_finished,omitempty"`
	LastError          string    `json:"last_error,omitempty"`
	LastBackup         string    `json:"last_backup,omitempty"`
	LastAttemptPath    string    `json:"last_attempt_path,omitempty"`
}

func (d *Daemon) readBackupSchedule(ctx context.Context) (BackupScheduleRecord, error) {
	var s BackupScheduleRecord
	raw, e := d.store.GetMarker(ctx, backupScheduleKey)
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
	if (s.State != "active" && s.State != "paused" && s.State != "blocked") || s.IntervalSeconds < 1 || s.IntervalSeconds > 31536000 || s.AuthorizingMessage == "" || s.NextAt.IsZero() || s.RunCount < 0 {
		return s, errors.New("invalid durable backup creation schedule")
	}
	return s, nil
}
func (d *Daemon) saveBackupSchedule(ctx context.Context, s BackupScheduleRecord, a controlplane.Audit) error {
	raw, e := json.Marshal(s)
	if e != nil {
		return e
	}
	return d.store.PutMarker(ctx, a, backupScheduleKey, raw)
}
func (d *Daemon) backupRecurrenceAuthority(ctx context.Context, message string) error {
	if e := d.store.AuthorizeReadOnlyEffect(ctx, message, "operator", "PlaneBackup", "runtime", actionClasses()); e != nil {
		return e
	}
	raw, e := d.store.MessageText(ctx, message)
	if e != nil {
		return e
	}
	var directed controlplane.DirectedMessage
	if e = json.Unmarshal([]byte(raw), &directed); e != nil {
		return errors.New("automatic backup creation requires explicit structured runtime or PlaneBackup grant")
	}
	for _, v := range directed.Authorize.Classes {
		if v == "runtime" {
			return nil
		}
	}
	for _, v := range directed.Authorize.Verbs {
		if v == "PlaneBackup" {
			return nil
		}
	}
	return errors.New("automatic backup creation requires explicit runtime or PlaneBackup grant")
}
func (d *Daemon) backupScheduleRPC(ctx context.Context, r api.Request) (any, error) {
	principal, ok := api.AuthenticatedPrincipal(ctx)
	if !ok || principal.Actor != "operator" || principal.Task != "" {
		return nil, errors.New("backup creation schedule requires instance operator")
	}
	var p BackupScheduleParams
	if e := decode(r.Params, &p); e != nil {
		return nil, e
	}
	if p.Action == "status" {
		return d.readBackupSchedule(ctx)
	}
	if p.Action != "configure" && p.Action != "pause" && p.Action != "resume" {
		return nil, errors.New("unknown backup creation schedule action")
	}
	if p.Action != "configure" && p.IntervalSeconds != 0 {
		return nil, errors.New("only configure accepts interval")
	}
	if e := d.store.AuthorizeReadOnlyEffect(ctx, p.AuthorizingMessage, "operator", "BackupSchedule", "runtime", actionClasses()); e != nil {
		return nil, e
	}
	if p.Action != "pause" {
		if e := d.backupRecurrenceAuthority(ctx, p.AuthorizingMessage); e != nil {
			return nil, e
		}
	}
	authorized, e := d.store.AuthorizeAction(ctx, p.AuthorizingMessage, "operator", "BackupSchedule", "runtime", false, actionClasses())
	if e != nil {
		return nil, e
	}
	if !d.backupScheduleMu.TryLock() {
		return nil, errors.New("backup creation schedule running; retry control after current run")
	}
	defer d.backupScheduleMu.Unlock()
	s, e := d.readBackupSchedule(ctx)
	if e != nil {
		return nil, e
	}
	switch p.Action {
	case "configure":
		if p.IntervalSeconds < 1 || p.IntervalSeconds > 31536000 {
			return nil, errors.New("interval_seconds must be 1..31536000")
		}
		s = BackupScheduleRecord{State: "active", IntervalSeconds: p.IntervalSeconds, AuthorizingMessage: p.AuthorizingMessage, NextAt: time.Now().UTC().Add(time.Duration(p.IntervalSeconds) * time.Second)}
	case "pause":
		if s.State == "unconfigured" {
			return nil, errors.New("backup creation schedule not configured")
		}
		s.State = "paused"
		s.Running = false
	case "resume":
		if s.State != "paused" && s.State != "blocked" {
			return nil, errors.New("only paused or blocked backup creation schedule can resume")
		}
		s.State = "active"
		s.Running = false
		s.LastError = ""
		s.AuthorizingMessage = p.AuthorizingMessage
		s.NextAt = time.Now().UTC().Add(time.Duration(s.IntervalSeconds) * time.Second)
	}
	e = d.saveBackupSchedule(authorized, s, controlplane.Audit{Actor: "operator", Action: "BackupSchedule", MessageID: p.AuthorizingMessage, Inputs: r.Params, Outcome: p.Action})
	return s, e
}

// Startup never repeats an interrupted backup creation automatically.
func (d *Daemon) recoverBackupSchedule(ctx context.Context) error {
	s, e := d.readBackupSchedule(ctx)
	if e != nil {
		return e
	}
	if !s.Running {
		return nil
	}
	s.State = "blocked"
	s.Running = false
	s.LastError = "interrupted backup creation attempt; inspect backups and explicitly resume"
	return d.saveBackupSchedule(ctx, s, controlplane.Audit{Actor: "daemon", Action: "backup creation schedule recovery", MessageID: s.AuthorizingMessage, Outcome: "blocked"})
}
func (d *Daemon) tickBackupSchedule(ctx context.Context, now time.Time) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	d.mu.Lock()
	draining := d.draining
	d.mu.Unlock()
	if draining {
		return nil
	}
	if _, e := os.Stat(filepath.Join(d.layout.Root, "maintenance")); e == nil {
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}

	if !d.backupScheduleMu.TryLock() {
		return nil
	}
	defer d.backupScheduleMu.Unlock()
	s, e := d.readBackupSchedule(ctx)
	if e != nil {
		return e
	}
	if s.State != "active" || s.Running || now.Before(s.NextAt) {
		return nil
	}
	authErr := d.backupRecurrenceAuthority(ctx, s.AuthorizingMessage)
	if !CanRunBackupSchedule(s.State == "active", !now.Before(s.NextAt), authErr == nil) {
		s.State = "blocked"
		s.LastError = fmt.Sprint(authErr)
		return d.saveBackupSchedule(ctx, s, controlplane.Audit{Actor: "daemon", Action: "backup creation schedule authority", MessageID: s.AuthorizingMessage, Outcome: "blocked"})
	}
	s.Running = true
	s.RunCount++
	s.LastStarted = now.UTC()
	s.LastError = ""
	s.LastAttemptPath = ""
	s.NextAt = now.UTC().Add(time.Duration(s.IntervalSeconds) * time.Second)
	audit := controlplane.Audit{Actor: "daemon", Action: "backup creation scheduled run", MessageID: s.AuthorizingMessage, Outcome: "claimed"}
	if e = d.saveBackupSchedule(ctx, s, audit); e != nil {
		return e
	}
	backup, runErr := d.backupPlane(context.WithValue(ctx, backupScheduleAuthorityKey{}, s.AuthorizingMessage))
	s.LastAttemptPath = backup.Path
	if runErr == nil {
		// Report only a backup that passed the offline restore and checksum checks.
		s.LastBackup = backup.Path
	}
	if runErr != nil && (backup.Path != "" || d.backupRecurrenceAuthority(ctx, s.AuthorizingMessage) != nil) {
		s.State = "blocked"
	}
	s.Running = false
	s.LastFinished = time.Now().UTC()
	audit.Outcome = "completed"
	if runErr != nil {
		s.LastError = runErr.Error()
		audit.Outcome = "failed"
	}
	// Cancellation can leave the durable claim running. Recovery blocks it.
	if e = d.saveBackupSchedule(ctx, s, audit); e != nil {
		return e
	}
	return runErr
}
func (d *Daemon) maintainBackupSchedule() {
	defer d.workers.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case now := <-ticker.C:
			ctx, cancel := context.WithTimeout(d.ctx, 2*time.Minute)
			_ = d.tickBackupSchedule(ctx, now)
			cancel()
		}
	}
}

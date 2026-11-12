package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
)

const replicationScheduleKey = "backup-replication-schedule"

type ReplicationScheduleParams struct {
	Action             string `json:"action"`
	IntervalSeconds    int64  `json:"interval_seconds,omitempty"`
	Endpoint           string `json:"endpoint,omitempty"`
	MaxBytes           int64  `json:"max_bytes,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}
type ReplicationSchedulePending struct {
	Backup         string `json:"backup"`
	Endpoint       string `json:"endpoint"`
	MaxBytes       int64  `json:"max_bytes"`
	ManifestDigest string `json:"manifest_digest"`
}
type ReplicationScheduleRecord struct {
	State              string                      `json:"state"`
	IntervalSeconds    int64                       `json:"interval_seconds"`
	Endpoint           string                      `json:"endpoint"`
	MaxBytes           int64                       `json:"max_bytes"`
	AuthorizingMessage string                      `json:"authorizing_message"`
	NextAt             time.Time                   `json:"next_at"`
	Running            bool                        `json:"running"`
	RunCount           int64                       `json:"run_count"`
	Pending            *ReplicationSchedulePending `json:"pending,omitempty"`
	LastStarted        time.Time                   `json:"last_started,omitempty"`
	LastFinished       time.Time                   `json:"last_finished,omitempty"`
	LastError          string                      `json:"last_error,omitempty"`
	LastBackup         string                      `json:"last_backup,omitempty"`
}

func (d *Daemon) readReplicationSchedule(ctx context.Context) (ReplicationScheduleRecord, error) {
	var s ReplicationScheduleRecord
	raw, e := d.store.GetMarker(ctx, replicationScheduleKey)
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
	p, e := normalizeReplicationParams(BackupReplicationParams{Backup: filepath.Join(d.layout.Root, "backup", "plane-schedule"), Endpoint: s.Endpoint, MaxBytes: s.MaxBytes})
	if e != nil || p.Endpoint != s.Endpoint || p.MaxBytes != s.MaxBytes || (s.State != "active" && s.State != "paused" && s.State != "blocked") || s.IntervalSeconds < 1 || s.IntervalSeconds > 31536000 || s.AuthorizingMessage == "" || s.NextAt.IsZero() || s.RunCount < 0 {
		return s, errors.New("invalid durable replication schedule")
	}
	if s.Pending != nil {
		c := s.Pending
		if filepath.Dir(c.Backup) != filepath.Join(d.layout.Root, "backup") || c.Endpoint != s.Endpoint || c.MaxBytes != s.MaxBytes || !replicationDigest(c.ManifestDigest) {
			return s, errors.New("replication pending claim differs from pinned configuration")
		}
	}
	return s, nil
}
func (d *Daemon) saveReplicationSchedule(ctx context.Context, s ReplicationScheduleRecord, a controlplane.Audit) error {
	raw, e := json.Marshal(s)
	if e != nil {
		return e
	}
	defer d.wakeReplicationSchedule()
	return d.store.PutMarker(ctx, a, replicationScheduleKey, raw)
}
func (d *Daemon) replicationRecurrenceAuthority(ctx context.Context, message string) error {
	for _, verb := range []string{"BackupReplicationSchedule", "BackupReplicate"} {
		if e := d.store.AuthorizeReadOnlyEffect(ctx, message, "operator", verb, "runtime", actionClasses()); e != nil {
			return e
		}
	}
	raw, e := d.store.MessageText(ctx, message)
	if e != nil {
		return e
	}
	var m controlplane.DirectedMessage
	if json.Unmarshal([]byte(raw), &m) != nil {
		return errors.New("automatic replication requires structured runtime or BackupReplicate grant")
	}
	for _, class := range m.Authorize.Classes {
		if class == "runtime" {
			return nil
		}
	}
	for _, verb := range m.Authorize.Verbs {
		if verb == "BackupReplicate" {
			return nil
		}
	}
	return errors.New("automatic replication requires explicit runtime or BackupReplicate grant")
}
func (d *Daemon) backupReplicationScheduleRPC(ctx context.Context, r api.Request) (any, error) {
	principal, ok := api.AuthenticatedPrincipal(ctx)
	if !ok || principal.Actor != "operator" || principal.Task != "" {
		return nil, errors.New("replication schedule requires instance operator")
	}
	var p ReplicationScheduleParams
	if e := decode(r.Params, &p); e != nil {
		return nil, e
	}
	if p.Action == "status" {
		return d.readReplicationSchedule(ctx)
	}
	if p.Action != "configure" && p.Action != "pause" && p.Action != "resume" {
		return nil, errors.New("unknown replication schedule action")
	}
	if p.Action != "configure" && (p.IntervalSeconds != 0 || p.Endpoint != "" || p.MaxBytes != 0) {
		return nil, errors.New("only configure accepts interval, endpoint and max_bytes")
	}
	if e := d.store.AuthorizeReadOnlyEffect(ctx, p.AuthorizingMessage, "operator", "BackupReplicationSchedule", "runtime", actionClasses()); e != nil {
		return nil, e
	}
	if p.Action != "pause" {
		if e := d.replicationRecurrenceAuthority(ctx, p.AuthorizingMessage); e != nil {
			return nil, e
		}
	}
	auth, e := d.store.AuthorizeAction(ctx, p.AuthorizingMessage, "operator", "BackupReplicationSchedule", "runtime", false, actionClasses())
	if e != nil {
		return nil, e
	}
	if !scheduleControlLock(ctx, &d.replicationScheduleMu, &d.replicationControls) {
		return nil, errors.New("replication schedule running; retry control after current run")
	}
	defer d.replicationScheduleMu.Unlock()
	s, e := d.readReplicationSchedule(ctx)
	if e != nil {
		return nil, e
	}
	switch p.Action {
	case "configure":
		if s.Pending != nil || s.Running {
			return nil, errors.New("pending replication must finish before reconfiguration")
		}
		if p.IntervalSeconds < 1 || p.IntervalSeconds > 31536000 {
			return nil, errors.New("interval_seconds must be 1..31536000")
		}
		normalized, e := normalizeReplicationParams(BackupReplicationParams{Backup: filepath.Join(d.layout.Root, "backup", "plane-schedule"), Endpoint: p.Endpoint, MaxBytes: p.MaxBytes})
		if e != nil {
			return nil, e
		}
		s = ReplicationScheduleRecord{State: "active", IntervalSeconds: p.IntervalSeconds, Endpoint: normalized.Endpoint, MaxBytes: normalized.MaxBytes, AuthorizingMessage: p.AuthorizingMessage, NextAt: time.Now().UTC().Add(time.Duration(p.IntervalSeconds) * time.Second)}
	case "pause":
		if s.State == "unconfigured" {
			return nil, errors.New("replication schedule not configured")
		}
		s.State = "paused"
		s.Running = false
	case "resume":
		if s.State != "paused" && s.State != "blocked" {
			return nil, errors.New("only paused or blocked replication schedule can resume")
		}
		s.State = "active"
		s.Running = false
		s.LastError = ""
		s.AuthorizingMessage = p.AuthorizingMessage
		s.NextAt = time.Now().UTC().Add(time.Duration(s.IntervalSeconds) * time.Second)
	}
	e = d.saveReplicationSchedule(auth, s, controlplane.Audit{Actor: "operator", Action: "BackupReplicationSchedule", MessageID: p.AuthorizingMessage, Inputs: r.Params, Outcome: p.Action})
	return s, e
}

// Startup performs no network calls and never discards an uncertain selection.
func (d *Daemon) recoverReplicationSchedule(ctx context.Context) error {
	s, e := d.readReplicationSchedule(ctx)
	if e != nil || !s.Running {
		return e
	}
	s.Running = false
	s.LastError = "interrupted replication; retained claim will reconcile the original outbox"
	return d.saveReplicationSchedule(ctx, s, controlplane.Audit{Actor: "daemon", Action: "replication schedule recovery", MessageID: s.AuthorizingMessage, Outcome: "retained"})
}
func (d *Daemon) replicationScheduleHeld() (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.draining {
		return true, nil
	}
	return d.maintenanceHeld()
}
func (d *Daemon) scheduleManifest(ctx context.Context, path string) (string, error) {
	if _, e := d.readBackup(ctx, path); e != nil {
		return "", e
	}
	marker := filepath.Join(path, "verified.json")
	if e := checkPrivateRegular(marker); e != nil {
		return "", e
	}
	raw, e := os.ReadFile(marker)
	if e != nil {
		return "", e
	}
	return controlplane.Digest(raw), nil
}

// Caller holds backupMu: oldest-first prevents a busy backup producer starving
// older snapshots. A source pin is durable before any selected claim is exposed.
func (d *Daemon) selectReplicationBackup(ctx context.Context, s ReplicationScheduleRecord, a controlplane.Audit) (*ReplicationSchedulePending, error) {
	if _, e := os.Lstat(filepath.Join(d.layout.Root, "backup")); os.IsNotExist(e) {
		return nil, nil
	}
	backups, e := d.retentionBackups(ctx)
	if e != nil {
		return nil, e
	}
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].Created.Equal(backups[j].Created) {
			return backups[i].Path < backups[j].Path
		}
		return backups[i].Created.Before(backups[j].Created)
	})
	for _, backup := range backups {
		digest, e := d.scheduleManifest(ctx, backup.Path)
		if e != nil {
			return nil, e
		}
		p := BackupReplicationParams{Backup: backup.Path, Endpoint: s.Endpoint, MaxBytes: s.MaxBytes}
		raw, e := d.store.GetMarker(ctx, replicationKey(p))
		if e == nil {
			var record BackupReplicationRecord
			if e = decode(raw, &record); e != nil {
				return nil, e
			}
			if record.Backup != p.Backup || record.Endpoint != p.Endpoint || record.MaxBytes != p.MaxBytes || record.ManifestSHA256 != digest {
				return nil, errors.New("existing replication outbox differs from schedule selection")
			}
			if record.State == "replicated" && record.Receipt != nil {
				receipt := record.Receipt
				if !replicationDigest(record.SHA256) || record.Size <= 0 || record.Size > record.MaxBytes || receipt.ID != record.SHA256 || receipt.SHA256 != record.SHA256 || receipt.ManifestSHA256 != digest || receipt.Size != record.Size || receipt.State != "stored" {
					return nil, errors.New("invalid confirmed replication proof")
				}
				continue
			}
			if record.State != "pending" {
				return nil, errors.New("invalid existing replication outbox")
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		if e = d.protectReplicationSource(ctx, backup.Path, a); e != nil {
			return nil, e
		}
		return &ReplicationSchedulePending{backup.Path, s.Endpoint, s.MaxBytes, digest}, nil
	}
	return nil, nil
}
func (d *Daemon) tickReplicationSchedule(ctx context.Context, now time.Time) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	held, e := d.replicationScheduleHeld()
	if e != nil || held {
		return e
	}
	if !scheduleRunLock(&d.replicationScheduleMu, &d.replicationControls) {
		return nil
	}
	defer d.replicationScheduleMu.Unlock()
	s, e := d.readReplicationSchedule(ctx)
	if e != nil {
		return e
	}
	if s.State != "active" || s.Running || now.Before(s.NextAt) {
		return nil
	}
	authErr := d.replicationRecurrenceAuthority(ctx, s.AuthorizingMessage)
	audit := controlplane.Audit{Actor: "daemon", Action: "replication scheduled run", MessageID: s.AuthorizingMessage, Outcome: "claimed"}
	if !CanRunReplicationSchedule(s.State == "active", !now.Before(s.NextAt), authErr == nil) {
		s.State = "blocked"
		s.LastError = fmt.Sprint(authErr)
		audit.Outcome = "blocked"
		return d.saveReplicationSchedule(ctx, s, audit)
	}

	release := func() error { return nil }
	if d.live != nil && d.live.guard != nil {
		sid := fmt.Sprintf("replication-schedule-select-%d-%d", now.UnixNano(), s.RunCount+1)
		var acquireErr error
		release, acquireErr = d.live.guard.AcquireHostIO(ctx, sid)
		if acquireErr != nil {
			return acquireErr
		}
	}
	if !d.backupMu.TryLock() {
		release()
		return nil
	}

	if s.Pending == nil {
		s.Pending, e = d.selectReplicationBackup(ctx, s, audit)
	}
	if e == nil && s.Pending != nil {
		var digest string
		digest, e = d.scheduleManifest(ctx, s.Pending.Backup)
		if e == nil && digest != s.Pending.ManifestDigest {
			e = errors.New("selected backup manifest changed")
		}
	}

	if e != nil {
		s.State = "blocked"
		s.Running = false
		s.LastError = e.Error()
		audit.Outcome = "blocked"
		saveErr := d.saveReplicationSchedule(ctx, s, audit)
		d.backupMu.Unlock()
		release()
		return errors.Join(e, saveErr)
	}
	unclaimed := s
	if e == nil {
		s.Running = s.Pending != nil
		s.RunCount++
		s.LastStarted = now.UTC()
		s.LastError = ""
		s.NextAt = now.UTC().Add(time.Duration(s.IntervalSeconds) * time.Second)
		e = d.saveReplicationSchedule(ctx, s, audit)
	}
	d.backupMu.Unlock()
	release()
	if e != nil {
		return e
	}
	if s.Pending == nil {
		return nil
	}
	runErr := d.replicationRecurrenceAuthority(ctx, s.AuthorizingMessage)
	if runErr == nil {
		held, runErr = d.replicationScheduleHeld()
		if held {
			runErr = errors.New("replication deferred by drain or maintenance")
		}
	}
	if runErr == nil {
		p := BackupReplicationParams{Backup: s.Pending.Backup, Endpoint: s.Pending.Endpoint, MaxBytes: s.Pending.MaxBytes, AuthorizingMessage: s.AuthorizingMessage}
		result, err := d.backupReplicate(context.WithValue(ctx, replicationScheduleContextKey{}, replicationScheduleContext{Claim: *s.Pending, Message: s.AuthorizingMessage}), p, audit)
		runErr = err
		if runErr == nil && result.ManifestSHA256 != s.Pending.ManifestDigest {
			runErr = errors.New("completed replication differs from selected manifest")
		}
		var deferral interface{ AdmissionDeferred() bool }
		if errors.As(runErr, &deferral) && deferral.AdmissionDeferred() {
			// Host admission deferred the upload before any write (FAE-HOST-002,
			// -010). Like a park it consumes no run: the selection stays durable
			// and a later tick retries it.
			unclaimed.LastError = runErr.Error()
			audit.Outcome = "deferred"
			if e = d.saveReplicationSchedule(ctx, unclaimed, audit); e != nil {
				return e
			}
			return runErr
		}
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
		audit.Outcome = "uncertain"
	} else {
		s.LastBackup = s.Pending.Backup
		s.Pending = nil
		s.LastError = ""
	}
	if e = d.saveReplicationSchedule(ctx, s, audit); e != nil {
		return e
	}
	return runErr
}

// Only scheduler code can attach this authority/identity constraint. It is
// rechecked inside the serialized transfer after resource waits, not from JSON.
type replicationScheduleContextKey struct{}
type replicationScheduleContext struct {
	Claim   ReplicationSchedulePending
	Message string
}

func (d *Daemon) validateScheduledReplication(ctx context.Context, p BackupReplicationParams, record *BackupReplicationRecord) error {
	pinned, ok := ctx.Value(replicationScheduleContextKey{}).(replicationScheduleContext)
	if !ok {
		return nil
	}
	if pinned.Claim.Backup != p.Backup || pinned.Claim.Endpoint != p.Endpoint || pinned.Claim.MaxBytes != p.MaxBytes {
		return errors.New("scheduled replication request drift")
	}
	if err := d.replicationRecurrenceAuthority(ctx, pinned.Message); err != nil {
		return err
	}
	if record != nil && record.ManifestSHA256 != pinned.Claim.ManifestDigest {
		return errors.New("scheduled replication archive differs from claim")
	}
	manifest, err := d.scheduleManifest(ctx, p.Backup)
	if err != nil {
		return err
	}
	if manifest != pinned.Claim.ManifestDigest {
		return errors.New("scheduled replication source differs from claim")
	}
	return nil
}

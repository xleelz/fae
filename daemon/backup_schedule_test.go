package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/hostguard"
)

func backupScheduleCall(t *testing.T, d *Daemon, p BackupScheduleParams) BackupScheduleRecord {
	t.Helper()
	response, e := call(t, d, "BackupSchedule", p, "")
	if e != nil {
		t.Fatal(e)
	}
	var s BackupScheduleRecord
	if e = json.Unmarshal(response.Result, &s); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestBackupScheduleGuard(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			for _, c := range []bool{false, true} {
				if CanRunBackupSchedule(a, b, c) != (a && b && c) {
					t.Fatal(a, b, c)
				}
			}
		}
	}
}
func TestBackupScheduleNativeLifecycleRestart(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	scheduleGrant(t, d, "creation")
	s := backupScheduleCall(t, d, BackupScheduleParams{Action: "configure", IntervalSeconds: 3600, AuthorizingMessage: "creation"})
	d.mu.Lock()
	d.draining = true
	d.mu.Unlock()
	if e = d.tickBackupSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	d.draining = false
	d.mu.Unlock()
	beginMaintenance(t, d)
	if e = d.tickBackupSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	if held, _ := d.maintenanceHeld(); !held {
		t.Fatal("controller maintenance marker not observed")
	}
	endMaintenance(t, d)
	s = backupScheduleCall(t, d, BackupScheduleParams{Action: "pause", AuthorizingMessage: "creation"})
	if e = d.tickBackupSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadDir(filepath.Join(d.layout.Root, "backup"))
	if e != nil || len(before) != 0 {
		t.Fatal(before, e)
	}
	stop(t, d)
	d = nil
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	s = backupScheduleCall(t, d, BackupScheduleParams{Action: "status"})
	if s.State != "paused" || s.RunCount != 0 {
		t.Fatal(s)
	}
	s = backupScheduleCall(t, d, BackupScheduleParams{Action: "resume", AuthorizingMessage: "creation"})
	due := s.NextAt.Add(48 * time.Hour)
	// A run deferred by host admission is withdrawn, so the concurrent pair is
	// retried until one tick runs; exactly one run is still asserted below.
	if e = tickAdmitted(t, "concurrent backup", func() error {
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- d.tickBackupSchedule(ctx, due) }()
		}
		wg.Wait()
		close(errs)
		var all []error
		for e := range errs {
			all = append(all, e)
		}
		return errors.Join(all...)
	}); e != nil {
		t.Fatal(e)
	}
	s = backupScheduleCall(t, d, BackupScheduleParams{Action: "status"})
	if s.RunCount != 1 || s.Running || s.LastBackup == "" || s.LastError != "" || !s.NextAt.After(due) {
		t.Fatal(s)
	}
	// The interval also runs from completion: no back-to-back runs.
	if s.NextAt.Before(s.LastFinished.Add(time.Duration(s.IntervalSeconds) * time.Second)) {
		t.Fatal("next run due before an interval after completion", s.NextAt, s.LastFinished)
	}
	if _, e = d.readBackup(ctx, s.LastBackup); e != nil {
		t.Fatal(e)
	}
	files, e := os.ReadDir(filepath.Join(d.layout.Root, "backup"))
	if e != nil || len(files) != 1 {
		t.Fatal(files, e)
	}
	s.Running = true
	raw, _ := json.Marshal(s)
	if e = d.store.PutMarker(ctx, controlplane.Audit{Actor: "operator", Action: "simulated backup claim", Outcome: "ok"}, backupScheduleKey, raw); e != nil {
		t.Fatal(e)
	}
	stop(t, d)
	d = nil
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	s = backupScheduleCall(t, d, BackupScheduleParams{Action: "status"})
	if s.State != "blocked" || s.Running || s.LastError == "" {
		t.Fatal(s)
	}
	if e = d.tickBackupSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	files, e = os.ReadDir(filepath.Join(d.layout.Root, "backup"))
	if e != nil || len(files) != 1 {
		t.Fatal("interrupted creation replay", files, e)
	}
}
func TestBackupScheduleNativeAuthorityFailureAndBusy(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	for _, g := range []struct{ id, text string }{{"plain", "create backups"}, {"restricted", `{"text":"only control","authorize":{"verbs":["BackupSchedule"]}}`}} {
		if e = d.store.RecordMessage(ctx, g.id, g.text); e != nil {
			t.Fatal(e)
		}
		if _, e = call(t, d, "BackupSchedule", BackupScheduleParams{Action: "configure", IntervalSeconds: 3600, AuthorizingMessage: g.id}, ""); e == nil {
			t.Fatal(g.id)
		}
	}
	scheduleGrant(t, d, "backupauth")
	for _, p := range []BackupScheduleParams{{Action: "configure", IntervalSeconds: 0, AuthorizingMessage: "backupauth"}, {Action: "configure", IntervalSeconds: 31536001, AuthorizingMessage: "backupauth"}, {Action: "pause", IntervalSeconds: 5, AuthorizingMessage: "backupauth"}, {Action: "resume", AuthorizingMessage: "backupauth"}} {
		if _, e = call(t, d, "BackupSchedule", p, ""); e == nil {
			t.Fatal(p)
		}
	}
	s := backupScheduleCall(t, d, BackupScheduleParams{Action: "configure", IntervalSeconds: 3600, AuthorizingMessage: "backupauth"})
	d.backupScheduleMu.Lock()
	_, e = call(t, d, "BackupSchedule", BackupScheduleParams{Action: "pause", AuthorizingMessage: "backupauth"}, "")
	d.backupScheduleMu.Unlock()
	if e == nil {
		t.Fatal("busy pause")
	}
	d.backupMu.Lock()
	e = d.tickBackupSchedule(ctx, s.NextAt)
	d.backupMu.Unlock()
	if e == nil {
		t.Fatal("busy backup accepted")
	}
	s = backupScheduleCall(t, d, BackupScheduleParams{Action: "status"})
	if s.State != "active" || s.LastAttemptPath != "" || s.LastError == "" {
		t.Fatal(s)
	}
	// Failure after allocating a backup directory blocks further creation.
	oldBinary := d.config.DoltBinary
	d.config.DoltBinary = "/missing-backup-verifier"
	e = tickAdmitted(t, "backup", func() error { return d.tickBackupSchedule(ctx, s.NextAt) })
	d.config.DoltBinary = oldBinary
	if e == nil {
		t.Fatal("verification failure ignored")
	}
	s = backupScheduleCall(t, d, BackupScheduleParams{Action: "status"})
	if s.State != "blocked" || s.LastAttemptPath == "" || s.LastBackup != "" {
		t.Fatal(s)
	}
	count := s.RunCount
	if e = d.tickBackupSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	s = backupScheduleCall(t, d, BackupScheduleParams{Action: "status"})
	if s.RunCount != count {
		t.Fatal("partial backup automatically retried")
	}
	s = backupScheduleCall(t, d, BackupScheduleParams{Action: "resume", AuthorizingMessage: "backupauth"})
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "operator", Action: "expire backup citation", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `UPDATE operator_messages SET created_at=? WHERE message_id=?`, time.Now().Add(-72*time.Hour), "backupauth")
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if e = d.tickBackupSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	s = backupScheduleCall(t, d, BackupScheduleParams{Action: "status"})
	if s.State != "blocked" || s.RunCount != count {
		t.Fatal(s)
	}
}
func TestBackupScheduleNativeBackground(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	scheduleGrant(t, d, "background-backup")
	backupScheduleCall(t, d, BackupScheduleParams{Action: "configure", IntervalSeconds: 1, AuthorizingMessage: "background-backup"})
	// Runs deferred by host admission are withdrawn and retried each tick; the
	// hang guard extends while the last attempt was deferred.
	guard := newHangGuard(15 * time.Second)
	for {
		s, e := d.readBackupSchedule(ctx)
		if e != nil {
			t.Fatal(e)
		}
		_, deferred := deferredRefusal(errors.New(s.LastError))
		if !guard.observe(deferred) {
			break
		}
		if s.RunCount > 0 && !s.Running {
			if s.LastError != "" || s.LastBackup == "" {
				t.Fatal(s)
			}
			if _, e = d.readBackup(ctx, s.LastBackup); e != nil {
				t.Fatal(e)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background backup not created")
}

func TestBackupScheduleNativeAuthorityExpiresDuringHostAdmission(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	scheduleGrant(t, d, "wait-grant")
	s := backupScheduleCall(t, d, BackupScheduleParams{Action: "configure", IntervalSeconds: 3600, AuthorizingMessage: "wait-grant"})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) {
		once.Do(func() { close(entered); <-release })
		return hostguard.Sample{Memory: hostguard.Reading{Value: 100 * float64(hostguard.GiB), Known: true}, Swap: hostguard.Reading{Value: 1, Known: true}, LoadPerCore: hostguard.Reading{Value: 1, Known: true}}, nil
	}
	finished := make(chan error, 1)
	go func() { finished <- d.tickBackupSchedule(ctx, s.NextAt) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("host admission not reached")
	}
	e = d.store.Mutate(ctx, controlplane.Audit{Actor: "operator", Action: "expire waiting backup grant", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE operator_messages SET created_at=? WHERE message_id=?`, time.Now().Add(-72*time.Hour), "wait-grant")
		return err
	})
	close(release)
	if e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-finished:
		if e == nil {
			t.Fatal("expired authority accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backup wait stuck")
	}
	s = backupScheduleCall(t, d, BackupScheduleParams{Action: "status"})
	if s.State != "blocked" || s.LastAttemptPath != "" || s.LastBackup != "" {
		t.Fatal(s)
	}
	files, e := os.ReadDir(filepath.Join(d.layout.Root, "backup"))
	if e != nil || len(files) != 0 {
		t.Fatal("backup allocated after grant expired", files, e)
	}
	assertHostCounts(t, d.live.guard, 0, 0)
}

// A scheduled run deferred by host admission is withdrawn, like a park: it
// consumes no run and no interval, allocates no backup, records the refusal,
// and a later tick runs it.
func TestBackupScheduleDeferredAdmissionConsumesNoRun(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	scheduleGrant(t, d, "deferred-backup")
	s := backupScheduleCall(t, d, BackupScheduleParams{Action: "configure", IntervalSeconds: 3600, AuthorizingMessage: "deferred-backup"})
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) { return hostSample(20, 0.5), nil }
	if reason, deferred := deferredRefusal(d.tickBackupSchedule(ctx, s.NextAt)); !deferred || reason != "IOPressure" {
		t.Fatal("pressure did not defer the run", reason)
	}
	after := backupScheduleCall(t, d, BackupScheduleParams{Action: "status"})
	if after.State != "active" || after.Running || after.RunCount != 0 || !after.NextAt.Equal(s.NextAt) || !strings.Contains(after.LastError, "host admission: IOPressure") || after.LastAttemptPath != "" {
		t.Fatal("deferred run consumed", after)
	}
	if files, e := os.ReadDir(filepath.Join(d.layout.Root, "backup")); e != nil || len(files) != 0 {
		t.Fatal("deferred run allocated a backup", files, e)
	}
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) { return hostSample(1, 0.5), nil }
	if e = d.tickBackupSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	after = backupScheduleCall(t, d, BackupScheduleParams{Action: "status"})
	if after.RunCount != 1 || after.LastError != "" || after.LastBackup == "" || !after.NextAt.After(s.NextAt) {
		t.Fatal("deferred run not retried", after)
	}
}

// A schedule whose runs outlast a short interval still waits a full interval
// after each completion, so its runs never follow each other back to back and
// a control or status read always finds it idle between runs.
func TestBackupScheduleIntervalRunsFromCompletion(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	scheduleGrant(t, d, "creation")
	backupScheduleCall(t, d, BackupScheduleParams{Action: "configure", IntervalSeconds: 1, AuthorizingMessage: "creation"})
	deadline := time.Now().Add(2 * time.Minute)
	for {
		s := backupScheduleCall(t, d, BackupScheduleParams{Action: "status"})
		if s.RunCount >= 1 && !s.Running {
			if s.NextAt.Before(s.LastFinished.Add(time.Second)) {
				t.Fatal("next run due before an interval after completion", s.NextAt, s.LastFinished)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no completed scheduled run", s)
		}
		time.Sleep(20 * time.Millisecond)
	}
	backupScheduleCall(t, d, BackupScheduleParams{Action: "pause", AuthorizingMessage: "creation"})
}

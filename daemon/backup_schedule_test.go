package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
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
	if e = os.WriteFile(filepath.Join(d.layout.Root, "maintenance"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if e = d.tickBackupSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	os.Remove(filepath.Join(d.layout.Root, "maintenance"))
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
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- d.tickBackupSchedule(ctx, due) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	s = backupScheduleCall(t, d, BackupScheduleParams{Action: "status"})
	if s.RunCount != 1 || s.Running || s.LastBackup == "" || s.LastError != "" || !s.NextAt.After(due) {
		t.Fatal(s)
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
	e = d.tickBackupSchedule(ctx, s.NextAt)
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
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		s, e := d.readBackupSchedule(ctx)
		if e != nil {
			t.Fatal(e)
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

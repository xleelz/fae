package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
)

func scheduleGrant(t *testing.T, d *Daemon, id string) {
	t.Helper()
	if e := d.store.RecordMessage(context.Background(), id, `{"text":"Run automatic backup retention and control its schedule","authorize":{"classes":["runtime"]}}`); e != nil {
		t.Fatal(e)
	}
}
func scheduleCall(t *testing.T, d *Daemon, p RetentionScheduleParams) RetentionScheduleRecord {
	t.Helper()
	response, e := call(t, d, "BackupRetentionSchedule", p, "")
	if e != nil {
		t.Fatal(e)
	}
	var s RetentionScheduleRecord
	if e = json.Unmarshal(response.Result, &s); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestRetentionScheduleGuard(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			for _, c := range []bool{false, true} {
				if CanRunRetentionSchedule(a, b, c) != (a && b && c) {
					t.Fatal(a, b, c)
				}
			}
		}
	}
}
func TestRetentionScheduleNativeLifecycleAndRestart(t *testing.T) {
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
	scheduleGrant(t, d, "schedule-grant")
	first, e := d.backupPlane(ctx)
	if e != nil {
		t.Fatal(e)
	}
	latest, e := d.backupPlane(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s := scheduleCall(t, d, RetentionScheduleParams{Action: "configure", Keep: 1, IntervalSeconds: 3600, AuthorizingMessage: "schedule-grant"})
	if s.State != "active" || s.RunCount != 0 {
		t.Fatal(s)
	}
	d.mu.Lock()
	d.draining = true
	d.mu.Unlock()
	if e = d.tickRetentionSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	d.draining = false
	d.mu.Unlock()
	if e = os.WriteFile(filepath.Join(d.layout.Root, "maintenance"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if e = d.tickRetentionSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(filepath.Join(d.layout.Root, "maintenance")); e != nil {
		t.Fatal(e)
	}
	current, e := d.readRetentionSchedule(ctx)
	if e != nil || current.RunCount != 0 {
		t.Fatal(current, e)
	}
	s = scheduleCall(t, d, RetentionScheduleParams{Action: "pause", AuthorizingMessage: "schedule-grant"})
	if s.State != "paused" {
		t.Fatal(s)
	}
	if e = d.tickRetentionSchedule(ctx, s.NextAt.Add(24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(first.Path); e != nil {
		t.Fatal("paused schedule removed backup")
	}
	stop(t, d)
	d = nil
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	s = scheduleCall(t, d, RetentionScheduleParams{Action: "status"})
	if s.State != "paused" {
		t.Fatal(s)
	}
	s = scheduleCall(t, d, RetentionScheduleParams{Action: "resume", AuthorizingMessage: "schedule-grant"})
	due := s.NextAt.Add(72 * time.Hour)
	if e = d.tickRetentionSchedule(ctx, due); e != nil {
		t.Fatal(e)
	}
	s = scheduleCall(t, d, RetentionScheduleParams{Action: "status"})
	if s.RunCount != 1 || s.Running || s.LastRemoved != 1 || s.LastError != "" || !s.NextAt.After(due) {
		t.Fatal(s)
	}
	if e = d.tickRetentionSchedule(ctx, due); e != nil {
		t.Fatal(e)
	}
	s = scheduleCall(t, d, RetentionScheduleParams{Action: "status"})
	if s.RunCount != 1 {
		t.Fatal("catchup burst", s)
	}
	if _, e = os.Stat(first.Path); !os.IsNotExist(e) {
		t.Fatal(e)
	}
	if _, e = d.readBackup(ctx, latest.Path); e != nil {
		t.Fatal(e)
	}
	// Crash after durable claim does not replay an old destructive plan.
	s.Running = true
	s.State = "active"
	raw, _ := json.Marshal(s)
	if e = d.store.PutMarker(ctx, controlplane.Audit{Actor: "operator", Action: "simulate claimed run", Outcome: "ok"}, retentionScheduleKey, raw); e != nil {
		t.Fatal(e)
	}
	stop(t, d)
	d = nil
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	s = scheduleCall(t, d, RetentionScheduleParams{Action: "status"})
	if s.State != "blocked" || s.Running || s.LastError == "" {
		t.Fatal(s)
	}
	s = scheduleCall(t, d, RetentionScheduleParams{Action: "resume", AuthorizingMessage: "schedule-grant"})
	if s.State != "active" {
		t.Fatal(s)
	}
}
func TestRetentionScheduleNativeAuthorityAndBusy(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	for _, grant := range []struct{ id, text string }{{"plain", "run retention"}, {"restricted", `{"text":"task only","authorize":{"verbs":["Admit"]}}`}} {
		if e = d.store.RecordMessage(ctx, grant.id, grant.text); e != nil {
			t.Fatal(e)
		}
		if _, e = call(t, d, "BackupRetentionSchedule", RetentionScheduleParams{Action: "configure", Keep: 1, IntervalSeconds: 3600, AuthorizingMessage: grant.id}, ""); e == nil {
			t.Fatal("weak grant accepted", grant.id)
		}
	}
	scheduleGrant(t, d, "expired")
	for _, p := range []RetentionScheduleParams{
		{Action: "configure", Keep: 0, IntervalSeconds: 1, AuthorizingMessage: "expired"},
		{Action: "configure", Keep: 1, IntervalSeconds: 0, AuthorizingMessage: "expired"},
		{Action: "configure", Keep: 1, IntervalSeconds: 31536001, AuthorizingMessage: "expired"},
		{Action: "resume", AuthorizingMessage: "expired"},
		{Action: "pause", Keep: 2, AuthorizingMessage: "expired"},
		{Action: "unknown", AuthorizingMessage: "expired"},
	} {
		if _, e = call(t, d, "BackupRetentionSchedule", p, ""); e == nil {
			t.Fatal("invalid lifecycle accepted", p)
		}
	}
	s := scheduleCall(t, d, RetentionScheduleParams{Action: "configure", Keep: 1, IntervalSeconds: 3600, AuthorizingMessage: "expired"})
	old := retainedFixtureBackup(t, d, "old", 1)
	retainedFixtureBackup(t, d, "new", 2)
	d.retentionScheduleMu.Lock()
	_, e = call(t, d, "BackupRetentionSchedule", RetentionScheduleParams{Action: "pause", AuthorizingMessage: "expired"}, "")
	d.retentionScheduleMu.Unlock()
	if e == nil {
		t.Fatal("pause claimed success during run")
	}
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "operator", Action: "expire citation", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `UPDATE operator_messages SET created_at=? WHERE message_id=?`, time.Now().Add(-72*time.Hour), "expired")
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if e = d.tickRetentionSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	s = scheduleCall(t, d, RetentionScheduleParams{Action: "status"})
	if s.State != "blocked" || s.RunCount != 0 {
		t.Fatal(s)
	}
	if _, e = os.Stat(old.Path); e != nil {
		t.Fatal("expired grant deleted backup")
	}
	scheduleGrant(t, d, "renewed")
	s = scheduleCall(t, d, RetentionScheduleParams{Action: "resume", AuthorizingMessage: "renewed"})
	// Missing/revoked citation fails closed on the next occurrence.
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "operator", Action: "remove citation fixture", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `DELETE FROM operator_messages WHERE message_id=?`, "renewed")
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if e = d.tickRetentionSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	s = scheduleCall(t, d, RetentionScheduleParams{Action: "status"})
	if s.State != "blocked" {
		t.Fatal(s)
	}
	if _, e = os.Stat(old.Path); e != nil {
		t.Fatal("missing grant deleted backup")
	}
}
func TestRetentionScheduleNativeBackground(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	scheduleGrant(t, d, "background")
	old := retainedFixtureBackup(t, d, "old", 1)
	retainedFixtureBackup(t, d, "new", 2)
	scheduleCall(t, d, RetentionScheduleParams{Action: "configure", Keep: 1, IntervalSeconds: 1, AuthorizingMessage: "background"})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s, e := d.readRetentionSchedule(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if s.RunCount > 0 && !s.Running {
			if s.LastError != "" || s.LastRemoved != 1 {
				t.Fatal(s)
			}
			if _, e = os.Stat(old.Path); !os.IsNotExist(e) {
				t.Fatal(e)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background schedule did not execute")
}

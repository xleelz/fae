package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/xleelz/fae/controlplane"
)

func quotaCall(t *testing.T, d *Daemon, p BackupQuotaParams) BackupQuotaStatus {
	t.Helper()
	r, e := call(t, d, "BackupQuota", p, "")
	if e != nil {
		t.Fatal(e)
	}
	var s BackupQuotaStatus
	if e = json.Unmarshal(r.Result, &s); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestBackupQuotaGuardAndAccounting(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			if CanAdmitBackupStorage(a, b) != (a && b) {
				t.Fatal(a, b)
			}
		}
	}
	d := retentionFixture(t)
	root := filepath.Join(d.layout.Root, "backup")
	partial := filepath.Join(root, "plane-partial")
	if e := os.Mkdir(partial, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(partial, "unfinished"), []byte("12345"), 0600); e != nil {
		t.Fatal(e)
	}
	spool := filepath.Join(d.layout.Root, "backup-replication")
	if e := os.Mkdir(spool, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(spool, "orphan"), []byte("1234567"), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := d.backupQuotaStatus(context.Background())
	if e != nil || s.UsageBytes != 12 || s.BackupCount != 1 || s.Exceeded {
		t.Fatal(s, e)
	}
	if e = os.Symlink(partial, filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	if _, e = d.backupQuotaStatus(context.Background()); e == nil {
		t.Fatal("symlink accepted")
	}
	if e = os.Remove(filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	if e = syscall.Mkfifo(filepath.Join(root, "pipe"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = d.backupQuotaStatus(context.Background()); e == nil {
		t.Fatal("special file accepted")
	}
	if e = os.Remove(filepath.Join(root, "pipe")); e != nil {
		t.Fatal(e)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = d.backupQuotaStatus(canceled); e == nil {
		t.Fatal("canceled accounting ignored")
	}
	if e = d.backupQuotaAdmission(context.Background(), -1, 0); e == nil {
		t.Fatal("negative reservation")
	}
}
func TestBackupQuotaNativeCountRestartAndAuthority(t *testing.T) {
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
	scheduleGrant(t, d, "quota")
	for _, g := range []string{"missing", "restricted"} {
		if g == "restricted" {
			if e = d.store.RecordMessage(ctx, g, `{"text":"task only","authorize":{"verbs":["Admit"]}}`); e != nil {
				t.Fatal(e)
			}
		}
		if _, e = call(t, d, "BackupQuota", BackupQuotaParams{Action: "configure", MaxBackups: 1, AuthorizingMessage: g}, ""); e == nil {
			t.Fatal("unauthorized quota")
		}
	}
	for _, p := range []BackupQuotaParams{{Action: "configure", MaxBytes: -1, AuthorizingMessage: "quota"}, {Action: "configure", MaxBytes: 1<<60 + 1, AuthorizingMessage: "quota"}, {Action: "configure", MaxBackups: 10001, AuthorizingMessage: "quota"}, {Action: "status", MaxBackups: 1}} {
		if _, e = call(t, d, "BackupQuota", p, ""); e == nil {
			t.Fatal(p)
		}
	}
	d.backupMu.Lock()
	_, e = call(t, d, "BackupQuota", BackupQuotaParams{Action: "configure", MaxBackups: 1, AuthorizingMessage: "quota"}, "")
	d.backupMu.Unlock()
	if e == nil {
		t.Fatal("quota reconfigured during backup")
	}
	quotaCall(t, d, BackupQuotaParams{Action: "configure", MaxBackups: 1, AuthorizingMessage: "quota"})
	first, e := d.backupPlane(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s := quotaCall(t, d, BackupQuotaParams{Action: "status"})
	if s.BackupCount != 1 || s.UsageBytes <= 0 || s.Exceeded {
		t.Fatal(s)
	}
	b, e := d.backupPlane(ctx)
	if e == nil || b.Path != "" {
		t.Fatal("quota allocated denied backup", b, e)
	}
	stop(t, d)
	d = nil
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	s = quotaCall(t, d, BackupQuotaParams{Action: "status"})
	if s.MaxBackups != 1 || s.BackupCount != 1 {
		t.Fatal(s)
	}
	quotaCall(t, d, BackupQuotaParams{Action: "configure", MaxBackups: 2, AuthorizingMessage: "quota"})
	if _, e = d.backupPlane(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = d.readBackup(ctx, first.Path); e != nil {
		t.Fatal("quota discarded existing backup", e)
	}
	s = quotaCall(t, d, BackupQuotaParams{Action: "configure", MaxBytes: 1, AuthorizingMessage: "quota"})
	if !s.Exceeded || s.BackupCount != 2 {
		t.Fatal(s)
	}
	s = quotaCall(t, d, BackupQuotaParams{Action: "configure", AuthorizingMessage: "quota"})
	if s.MaxBytes != 0 || s.MaxBackups != 0 || s.Exceeded {
		t.Fatal("disable failed", s)
	}
}
func TestBackupQuotaNativeExportOverrunPreserved(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	scheduleGrant(t, d, "quota")
	quotaCall(t, d, BackupQuotaParams{Action: "configure", MaxBytes: 1, AuthorizingMessage: "quota"})
	b, e := d.backupPlane(ctx)
	if e == nil || b.Path == "" || !strings.Contains(e.Error(), "quota") {
		t.Fatal(b, e)
	}
	if _, e = os.Stat(b.Path); e != nil {
		t.Fatal("overrun removed data", e)
	}
	s := quotaCall(t, d, BackupQuotaParams{Action: "status"})
	if !s.Exceeded || s.BackupCount != 1 || s.UsageBytes <= 1 {
		t.Fatal(s)
	}
	if _, e = d.readBackup(ctx, b.Path); e == nil {
		t.Fatal("unverified export certified")
	}
	denied, e := d.backupPlane(ctx)
	if e == nil || denied.Path != "" {
		t.Fatal("overrun admitted another backup", denied, e)
	}
}
func TestBackupQuotaNativeReplicationSpoolBound(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	scheduleGrant(t, d, "quota")
	path := replicationFixtureBackup(t, d)
	s := quotaCall(t, d, BackupQuotaParams{Action: "status"})
	quotaCall(t, d, BackupQuotaParams{Action: "configure", MaxBytes: s.UsageBytes + 100, AuthorizingMessage: "quota"})
	file, _, e := d.prepareReplicationArchive(ctx, BackupReplicationParams{Backup: path, Endpoint: "https://example.com", MaxBytes: 1 << 20})
	if e == nil || file != nil {
		t.Fatal("spool quota ignored", e)
	}
	after := quotaCall(t, d, BackupQuotaParams{Action: "status"})
	if after.UsageBytes != s.UsageBytes {
		t.Fatal("failed spool leaked", s, after)
	}
	entries, e := os.ReadDir(filepath.Join(d.layout.Root, "backup-replication"))
	if e != nil || len(entries) != 0 {
		t.Fatal(entries, e)
	}
	if _, e = d.readBackup(ctx, path); e != nil {
		t.Fatal("spool failure damaged backup", e)
	}
	quotaCall(t, d, BackupQuotaParams{Action: "configure", MaxBytes: s.UsageBytes, AuthorizingMessage: "quota"})
	if remaining, e := d.backupQuotaRemaining(ctx); e != nil || remaining != 0 {
		t.Fatal(remaining, e)
	}
	// Malformed durable limits fail closed rather than silently disabling quotas.
	if e = d.store.PutMarker(ctx, controlplane.Audit{Actor: "operator", Action: "quota corruption fixture", Outcome: "ok"}, backupQuotaKey, json.RawMessage(`{"max_bytes":-1,"max_backups":0}`)); e != nil {
		t.Fatal(e)
	}
	if _, e = d.backupQuotaStatus(ctx); e == nil {
		t.Fatal("corrupt quota accepted")
	}
}

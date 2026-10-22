package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/daemon"
	"os"
	"testing"
)

func TestCleanupCLI(t *testing.T) {
	home := t.TempDir()
	layout, err := daemon.Paths(home, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(layout.Run, 0700); err != nil {
		t.Fatal(err)
	}
	token, err := api.Credential(layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan api.Request, 1)
	server, err := api.New(token, func(_ context.Context, r api.Request) (any, error) {
		requests <- r
		return map[string]bool{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() {
		server.Shutdown(context.Background())
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for _, tt := range []struct {
		args   []string
		method string
		apply  bool
	}{{[]string{"backup", "plane"}, "PlaneBackup", false}, {[]string{"cleanup", "retired"}, "CleanupRetired", false}, {[]string{"cleanup", "retired", "--dry-run"}, "CleanupRetired", false}, {[]string{"cleanup", "retired", "--apply", "--backup", "/owned/plane-x", "--pace", "500ms"}, "CleanupRetired", true}} {
		args := append(tt.args, "--home", home, "--instance", "test", "--authorizing-message", "m")
		if err = execute(context.Background(), args, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		r := <-requests
		var p daemon.CleanupParams
		if err = json.Unmarshal(r.Params, &p); err != nil {
			t.Fatal(err)
		}
		if r.Method != tt.method || p.Apply != tt.apply || p.AuthorizingMessage != "m" {
			t.Fatal(r, p)
		}
		if tt.apply && (p.Backup != "/owned/plane-x" || p.PaceMS != 500) {
			t.Fatal(p)
		}
	}
	for _, tt := range []struct {
		args   []string
		method string
		fields map[string]any
	}{
		{[]string{"backup", "unpin", "--backup", "/owned/plane-x"}, "BackupReplicationUnpin", map[string]any{"backup": "/owned/plane-x", "apply": false}},
		{[]string{"backup", "unpin", "--backup", "/owned/plane-x", "--apply", "--plan-digest", "digest"}, "BackupReplicationUnpin", map[string]any{"apply": true, "plan_digest": "digest"}},
		{[]string{"backup", "quota", "status"}, "BackupQuota", map[string]any{"action": "status"}},
		{[]string{"backup", "quota", "configure", "--max-bytes", "1048576", "--max-backups", "5"}, "BackupQuota", map[string]any{"action": "configure", "max_bytes": float64(1048576), "max_backups": float64(5)}},
		{[]string{"backup", "quota", "configure"}, "BackupQuota", map[string]any{"action": "configure", "max_bytes": float64(0), "max_backups": float64(0)}},
		{[]string{"backup", "creation-schedule", "configure"}, "BackupSchedule", map[string]any{"action": "configure", "interval_seconds": float64(3600)}},
		{[]string{"backup", "creation-schedule", "configure", "--interval", "2s"}, "BackupSchedule", map[string]any{"action": "configure", "interval_seconds": float64(2)}},
		{[]string{"backup", "creation-schedule", "pause"}, "BackupSchedule", map[string]any{"action": "pause"}},
		{[]string{"backup", "creation-schedule", "resume"}, "BackupSchedule", map[string]any{"action": "resume"}},
		{[]string{"backup", "creation-schedule", "status"}, "BackupSchedule", map[string]any{"action": "status"}},
		{[]string{"backup", "replication-schedule", "configure", "--endpoint", "https://replica.example"}, "BackupReplicationSchedule", map[string]any{"action": "configure", "interval_seconds": float64(3600), "endpoint": "https://replica.example", "max_bytes": float64(0)}},
		{[]string{"backup", "replication-schedule", "configure", "--interval", "3s", "--endpoint", "http://127.0.0.1:1234", "--max-bytes", "1048576"}, "BackupReplicationSchedule", map[string]any{"action": "configure", "interval_seconds": float64(3), "max_bytes": float64(1048576)}},
		{[]string{"backup", "replication-schedule", "pause"}, "BackupReplicationSchedule", map[string]any{"action": "pause"}},
		{[]string{"backup", "replication-schedule", "resume"}, "BackupReplicationSchedule", map[string]any{"action": "resume"}},
		{[]string{"backup", "replication-schedule", "status"}, "BackupReplicationSchedule", map[string]any{"action": "status"}},
		{[]string{"backup", "retention-schedule", "configure"}, "BackupRetentionSchedule", map[string]any{"action": "configure", "interval_seconds": float64(3600), "keep": float64(3)}},
		{[]string{"backup", "retention-schedule", "configure", "--interval", "2s", "--keep", "4"}, "BackupRetentionSchedule", map[string]any{"action": "configure", "interval_seconds": float64(2), "keep": float64(4)}},
		{[]string{"backup", "retention-schedule", "pause"}, "BackupRetentionSchedule", map[string]any{"action": "pause"}},
		{[]string{"backup", "retention-schedule", "resume"}, "BackupRetentionSchedule", map[string]any{"action": "resume"}},
		{[]string{"backup", "retention-schedule", "status"}, "BackupRetentionSchedule", map[string]any{"action": "status"}},
		{[]string{"backup", "retention", "--keep", "2"}, "BackupRetention", map[string]any{"keep": float64(2), "apply": false}},
		{[]string{"backup", "retention", "--keep", "3", "--apply", "--plan-digest", "digest"}, "BackupRetention", map[string]any{"keep": float64(3), "apply": true, "plan_digest": "digest"}},
		{[]string{"backup", "replicate", "--backup", "/owned/plane-x", "--endpoint", "https://replica.example"}, "BackupReplicate", map[string]any{"backup": "/owned/plane-x", "endpoint": "https://replica.example"}},
	} {
		args := append(tt.args, "--home", home, "--instance", "test", "--authorizing-message", "m")
		if err = execute(context.Background(), args, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		r := <-requests
		var params map[string]any
		if err = json.Unmarshal(r.Params, &params); err != nil {
			t.Fatal(err)
		}
		if r.Method != tt.method || params["authorizing_message"] != "m" {
			t.Fatal(r, params)
		}
		for k, v := range tt.fields {
			if params[k] != v {
				t.Fatal(k, params)
			}
		}
	}
	for _, args := range [][]string{{"backup", "quota"}, {"backup", "quota", "invalid"}, {"backup", "quota", "configure", "--max-bytes", "-1"}, {"backup", "quota", "configure", "--max-bytes", "1152921504606846977"}, {"backup", "quota", "configure", "--max-backups", "10001"}, {"backup", "quota", "configure", "--max-backups", "-1"}, {"backup", "quota", "status", "--max-bytes", "1"}, {"backup", "unpin"}, {"backup", "unpin", "--backup", "/x", "--apply"}, {"backup", "unpin", "--backup", "/x", "--apply", "--dry-run", "--plan-digest", "x"}, {"backup", "creation-schedule"}, {"backup", "creation-schedule", "invalid"}, {"backup", "creation-schedule", "configure", "--interval", "500ms"}, {"backup", "creation-schedule", "pause", "--interval", "2s"}, {"backup", "replication-schedule", "configure"}, {"backup", "replication-schedule", "configure", "--endpoint", "https://replica.example", "--max-bytes", "100"}, {"backup", "replication-schedule", "configure", "--endpoint", "https://replica.example", "--max-bytes", "-1"}, {"backup", "retention-schedule"}, {"backup", "retention-schedule", "invalid"}, {"backup", "retention-schedule", "configure", "--interval", "500ms"}, {"backup", "retention-schedule", "configure", "--keep", "0"}, {"backup", "retention-schedule", "pause", "--keep", "2"}, {"cleanup", "scratch-archive"}, {"cleanup", "scratch-archive", "--source", "/x", "--survivor", "/y", "--apply"}, {"cleanup", "scratch-archive", "--source", "/x", "--survivor", "/y", "--apply", "--dry-run", "--plan-digest", "x"}, {"backup", "retention", "--keep", "0"}, {"backup", "retention", "--apply"}, {"backup", "retention", "--apply", "--dry-run", "--plan-digest", "x"}, {"backup", "replicate"}, {"backup", "replicate", "--backup", "/x"}, {"cleanup"}, {"cleanup", "unknown"}, {"backup", "unknown"}, {"cleanup", "retired", "--pace", "1ms"}, {"cleanup", "retired", "--pace", "2m"}} {
		if execute(context.Background(), args, &bytes.Buffer{}) == nil {
			t.Fatal("invalid arguments", args)
		}
	}
}

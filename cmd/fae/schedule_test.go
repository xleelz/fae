package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/daemon"
	"os"
	"path/filepath"
	"testing"
)

func TestCronCLIFile(t *testing.T) {
	home := t.TempDir()
	layout, e := daemon.Paths(home, "cron-test")
	if e != nil {
		t.Fatal(e)
	}
	os.MkdirAll(layout.Run, 0700)
	credential, e := api.Credential(layout.Credential)
	if e != nil {
		t.Fatal(e)
	}
	var received daemon.ScheduleParams
	server, e := api.New(credential, func(_ context.Context, r api.Request) (any, error) {
		if r.Method != "Schedule" {
			t.Error(r.Method)
		}
		e := json.Unmarshal(r.Params, &received)
		return map[string]bool{"ok": true}, e
	})
	if e != nil {
		t.Fatal(e)
	}
	if e = server.Listen(layout.Socket); e != nil {
		t.Fatal(e)
	}
	go server.Serve()
	defer server.Shutdown(context.Background())
	file := filepath.Join(home, "schedule.json")
	base := `{"id":"file-id","kind":"cron","anchor":"2026-10-18T01:02:03Z","interval_seconds":60,"template":{"workflow_id":"default","revision":0}}`
	if e = os.WriteFile(file, []byte(base), 0600); e != nil {
		t.Fatal(e)
	}
	args := []string{"schedule", "create", "--kind", "cron", "--spec", file, "--home", home, "--instance", "cron-test", "--authorizing-message", "m"}
	if e = execute(context.Background(), args, &bytes.Buffer{}); e != nil {
		t.Fatal(e)
	}
	if received.Cron == nil || received.Cron.ID != "file-id" || received.Cron.Template.Revision != 0 || received.Spec != nil {
		t.Fatal(received)
	}
	for _, bad := range []string{base + ` {}`, `{"id":"a","kind":"cron","unknown":true}`, `{"kind":"loop"}`, `{"kind":"cron","kind":"loop"}`} {
		os.WriteFile(file, []byte(bad), 0600)
		if e = execute(context.Background(), args, &bytes.Buffer{}); e == nil || exitCode(e) != 2 {
			t.Fatal("malformed specification accepted", bad, e)
		}
	}
}

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
	"time"
)

func TestCronCLIFile(t *testing.T) {
	home := shortHome(t)
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
	if received.Cron == nil || received.Cron.ID != "file-id" || received.Cron.Template.Revision != 0 || received.Cron.Template.Priority != 10 || received.Spec != nil {
		t.Fatal(received)
	}
	for _, bad := range []string{
		base + ` {}`,
		`{"id":"a","kind":"cron","unknown":true}`,
		`{"kind":"loop"}`,
		`{"kind":"cron","kind":"loop"}`,
		`{"id":"file-id","kind":"cron","anchor":"2026-10-18T01:02:03Z","interval_seconds":60,"template":{"workflow_id":"default","revision":0,"priority":0}}`,
		`{"id":"file-id","kind":"cron","anchor":"2026-10-18T01:02:03Z","interval_seconds":60,"template":{"workflow_id":"default","revision":0,"priority":9}}`,
	} {
		os.WriteFile(file, []byte(bad), 0600)
		if e = execute(context.Background(), args, &bytes.Buffer{}); e == nil || exitCode(e) != 1 {
			t.Fatal("malformed specification not refused as a validation failure", bad, e)
		}
	}
	for _, low := range []string{"0", "9", "-1"} {
		flagArgs := []string{"schedule", "create", "--kind", "cron", "--id", "flag-id", "--anchor", "2026-10-18T01:02:03Z", "--interval-seconds", "60", "--priority", low, "--home", home, "--instance", "cron-test", "--authorizing-message", "m"}
		if e = execute(context.Background(), flagArgs, &bytes.Buffer{}); exitCode(e) != 2 {
			t.Fatalf("--priority %s: got %v (exit %d), want exit 2", low, e, exitCode(e))
		}
	}
}

// FAE-CLI-002: a loop's expiry is explicit or taken from the daemon clock;
// the CLI never computes it from its own clock.
func TestLoopExpiryUsesDaemonClockOrExplicitTime(t *testing.T) {
	home, requests := recordingDaemon(t, "loop", map[string]any{"ok": true})
	run := func(args ...string) error {
		return execute(context.Background(), append(append([]string{"schedule", "create", "--id", "l", "--role", "oracle", "--prompt", "p"}, args...), "--home", home, "--instance", "loop", "--authorizing-message", "m"), &bytes.Buffer{})
	}
	if err := run("--expires-in", "2h"); err != nil {
		t.Fatal(err)
	}
	var p daemon.ScheduleParams
	if err := json.Unmarshal((<-requests).Params, &p); err != nil || p.ExpiresInSeconds != 7200 || !p.Spec.ExpiresAt.IsZero() {
		t.Fatalf("daemon-clock expiry %+v %v", p, err)
	}
	if err := run("--expires-at", "2027-01-02T03:04:05Z"); err != nil {
		t.Fatal(err)
	}
	p = daemon.ScheduleParams{}
	if err := json.Unmarshal((<-requests).Params, &p); err != nil || p.ExpiresInSeconds != 0 || !p.Spec.ExpiresAt.Equal(time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("explicit expiry %+v %v", p, err)
	}
	// An out-of-range --expires-in falls back to the 7-day default
	// (TestConfiguredValuesFallBackOnEveryVerb); a malformed one is usage.
	for _, bad := range [][]string{{"--expires-at", "tomorrow"}, {"--expires-at", "2027-01-02T03:04:05Z", "--expires-in", "1h"}, {"--expires-in", "7d"}, {"--expires-in", "soon"}} {
		if err := run(bad...); exitCode(err) != 2 {
			t.Fatalf("%v: %v", bad, err)
		}
	}
}

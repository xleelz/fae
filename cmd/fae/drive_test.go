package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/daemon"
)

func TestDriveCLI(t *testing.T) {
	home := t.TempDir()
	layout, e := daemon.Paths(home, "drive-test")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(layout.Run, 0700); e != nil {
		t.Fatal(e)
	}
	token, e := api.Credential(layout.Credential)
	if e != nil {
		t.Fatal(e)
	}
	received := make(chan controller.Params, 4)
	server, e := api.New(token, func(_ context.Context, r api.Request) (any, error) {
		if r.Method != "Controller" {
			t.Errorf("method %s", r.Method)
		}
		var p controller.Params
		if e := json.Unmarshal(r.Params, &p); e != nil {
			return nil, e
		}
		received <- p
		return controller.Health{PID: 123, DriveGeneration: 2}, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if e = server.Listen(layout.Socket); e != nil {
		t.Fatal(e)
	}
	go server.Serve()
	defer server.Shutdown(context.Background())
	for verb, want := range map[string]string{"stop": "stop-drive", "rearm": "rearm", "status": "health"} {
		var output bytes.Buffer
		if e = execute(context.Background(), []string{"drive", verb, "--home", home, "--instance", "drive-test", "--authorizing-message", "message"}, &output); e != nil {
			t.Fatal(e)
		}
		p := <-received
		if p.Action != want || p.AuthorizingMessage != "message" {
			t.Fatal(p)
		}
		var h controller.Health
		if e = json.Unmarshal(output.Bytes(), &h); e != nil || h.DriveGeneration != 2 || h.PID != 123 {
			t.Fatal(h, e)
		}
	}
	if e = execute(context.Background(), []string{"watchdog", "--revoke-grant", "old-message", "--home", home, "--instance", "drive-test", "--authorizing-message", "fresh-message"}, &bytes.Buffer{}); e != nil {
		t.Fatal(e)
	}
	revoked := <-received
	if revoked.Action != "watchdog-revoke" || revoked.GrantMessage != "old-message" || revoked.AuthorizingMessage != "fresh-message" {
		t.Fatal(revoked)
	}
	if e = execute(context.Background(), []string{"drive", "invalid", "--home", home, "--instance", "drive-test"}, &bytes.Buffer{}); e == nil {
		t.Fatal("invalid drive action accepted")
	}
}

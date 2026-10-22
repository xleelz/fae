package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/daemon"
)

func TestTaskControlCLIRefusesMissingAndMalformedFlags(t *testing.T) {
	for _, args := range [][]string{{"cancel"}, {"escalate", "--task", "t"}, {"defer", "--task", "t", "--authorizing-message", "m", "--until", "bad"}, {"defer", "--task", "t", "--authorizing-message", "m", "--undefer", "--until", "2027-01-01T00:00:00Z"}, {"cancel", "--task", "t", "--authorizing-message", "m", "--undefer"}} {
		if err := execute(context.Background(), args, &bytes.Buffer{}); err == nil || exitCode(err) != 2 {
			t.Fatal(args, err)
		}
	}
}

func TestTaskControlCLIThinRPCs(t *testing.T) {
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
	}{{[]string{"retire", "--reason", "obsolete ruling/123"}, "Retire"}, {[]string{"cancel", "--reason", "no longer needed"}, "Cancel"}, {[]string{"escalate", "--reason", "needs owner"}, "Escalate"}, {[]string{"block", "--reason", "dependency"}, "Block"}, {[]string{"defer", "--until", "2027-01-01T00:00:00Z"}, "Defer"}, {[]string{"defer", "--undefer"}, "Undefer"}, {[]string{"reprioritize", "--priority", "100"}, "Reprioritize"}, {[]string{"set-attempt-limit", "--limit", "25"}, "SetAttemptLimit"}, {[]string{"prophet", "restart", "--information", "new requirement"}, "RestartWithInformation"}, {[]string{"prophet", "resume", "--context", "later turn"}, "ResumeWithContext"}, {[]string{"resume"}, "Resume"}} {
		args := append(tt.args, "--home", home, "--instance", "test", "--task", "task", "--authorizing-message", "m")
		if err := execute(context.Background(), args, &bytes.Buffer{}); err != nil {
			t.Fatal(tt.method, err)
		}
		r := <-requests
		var p daemon.TaskControlParams
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatal(err)
		}
		if r.Method != tt.method || p.ID != "task" || p.AuthorizingMessage != "m" {
			t.Fatal("incorrect RPC", r, p)
		}
	}
}

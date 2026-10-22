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

func TestWorkspaceCLI(t *testing.T) {
	home := t.TempDir()
	layout, err := daemon.Paths(home, "test")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(layout.Run, 0700)
	token, err := api.Credential(layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	calls := make(chan api.Request, 2)
	server, err := api.New(token, func(_ context.Context, r api.Request) (any, error) {
		calls <- r
		return map[string]string{"state": "ready"}, nil
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
	for _, action := range []string{"review-plan", "review-prepare"} {
		args := []string{"workspace", action, "--task", "task", "--run-id", "run", "--home", home, "--instance", "test"}
		if action == "review-prepare" {
			args = append(args, "--adapter", "fixture", "--adapter-revision", "2", "--adapter-hash", "hash", "--authorizing-message", "message")
		}
		if err = execute(context.Background(), args, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		r := <-calls
		var p daemon.ReviewWorkspaceParams
		if err = json.Unmarshal(r.Params, &p); err != nil {
			t.Fatal(err)
		}
		if p.Task != "task" || p.RunID != "run" {
			t.Fatal(p)
		}
		if action == "review-plan" && r.Method != "ReviewWorkspacePlan" {
			t.Fatal(r)
		}
		if action == "review-prepare" && (r.Method != "ReviewWorkspacePrepare" || p.Adapter.Revision != 2 || p.AuthorizingMessage != "message") {
			t.Fatal(r, p)
		}
	}
	for _, args := range [][]string{{"workspace"}, {"workspace", "wrong"}, {"workspace", "review-plan"}, {"workspace", "review-prepare", "--task", "t"}, {"workspace", "review-plan", "--task", "t", "--adapter-revision", "-1"}} {
		if execute(context.Background(), args, &bytes.Buffer{}) == nil {
			t.Fatal(args)
		}
	}
}

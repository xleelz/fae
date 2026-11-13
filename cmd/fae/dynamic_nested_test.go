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

func TestRuntimeConfigNestedPolicyRequiresAuthorityAndTypedValue(t *testing.T) {
	home := shortHome(t)
	layout, err := daemon.Paths(home, "nested")
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
	go server.Serve()
	defer server.Shutdown(context.Background())
	base := []string{"--home", home, "--instance", "nested", "runtime-config", "--nested-commands"}
	for _, args := range [][]string{append(append([]string{}, base...), "account"), append(append([]string{}, base...), "allow", "--authorizing-message", "message")} {
		if err = execute(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatal("invalid/unauthorized policy accepted")
		}
	}
	select {
	case <-requests:
		t.Fatal("invalid command reached daemon")
	default:
	}
	args := append(append([]string{}, base...), "account", "--authorizing-message", "message")
	if err = execute(context.Background(), args, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	r := <-requests
	var p daemon.RuntimeConfig
	if err = json.Unmarshal(r.Params, &p); err != nil || r.Method != "RuntimeConfig" || p.NestedCommands != "account" || p.AuthorizingMessage != "message" {
		t.Fatal(r, p, err)
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/daemon"
)

func TestCLICodes(t *testing.T) {
	home := t.TempDir()
	for _, tt := range []struct {
		args []string
		code int
	}{
		{[]string{"unknown"}, 2},
		{[]string{"resolve-uncertain", "--id", "t", "--dispatch", "d", "--action", "bogus", "--authorizing-message", "m"}, 2},
		{[]string{"runtime-config", "--pane-limit", "20"}, 2},
		{[]string{"runtime-config", "--heavy-capacity", "-1", "--authorizing-message", "m"}, 2},
		{[]string{"runtime-config", "--tasks-in-flight", "257", "--authorizing-message", "m"}, 2},
		{[]string{"runtime-config", "--host-thresholds", "null", "--authorizing-message", "m"}, 2},
		{[]string{"runtime-config", "--pane-limit", "1", "--authorizing-message", "m"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "scripted", "--message-max-age", "0s"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "scripted", "--message-max-age", "-1s"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "scripted", "--message-max-age", "invalid"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "direct"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "direct", "--scenario", "file"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "scripted", "--live-config", "file"}, 2}, {[]string{"run", "--priority", "bad"}, 2}, {[]string{"run", "--prompt", "x"}, 1},
		{[]string{"run", "--prompt", "x", "--priority", "9", "--authorizing-message", "m"}, 1},
		{[]string{"serve", "--workspace", home}, 1},
		{[]string{"status", "--home", home}, 3},
		{[]string{"message", "--text", "x", "--file", "missing"}, 2},
	} {
		err := execute(context.Background(), tt.args, &bytes.Buffer{})
		if err == nil || exitCode(err) != tt.code {
			t.Errorf("%v: %v code=%d want=%d", tt.args, err, exitCode(err), tt.code)
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("unreachable read created instance files")
	}
}
func TestCLI(t *testing.T) {
	home := t.TempDir()
	layout, err := daemon.Paths(home, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(layout.Run, 0700); err != nil {
		t.Fatal(err)
	}
	credential, err := api.Credential(layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan api.Request, 8)
	server, err := api.New(credential, func(_ context.Context, r api.Request) (any, error) {
		requests <- r
		return map[string]any{"ok": true}, nil
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
	commands := []struct {
		args   []string
		method string
	}{
		{[]string{"schedule", "create", "--home", home, "--instance", "demo", "--id", "loop", "--role", "oracle", "--prompt", "continue", "--max-iterations", "5", "--authorizing-message", "m"}, "Schedule"},
		{[]string{"runtime-config", "--home", home, "--instance", "demo", "--pane-limit", "20", "--authorizing-message", "m"}, "RuntimeConfig"},
		{[]string{"resolve-uncertain", "--home", home, "--instance", "demo", "--id", "t", "--dispatch", "d", "--action", "redispatch", "--authorizing-message", "m"}, "ResolveUncertain"},
		{[]string{"resolve-uncertain", "--home", home, "--instance", "demo", "--id", "t", "--dispatch", "d", "--action", "accept", "--authorizing-message", "m"}, "ResolveUncertain"},
		{[]string{"runtime-config", "--home", home, "--instance", "demo", "--heavy-capacity", "3", "--tasks-in-flight", "5", "--authorizing-message", "m"}, "RuntimeConfig"},
		{[]string{"--home", home, "--instance", "demo", "status", "--json"}, "Status"},
		{[]string{"provider-health", "--home", home, "--instance", "demo"}, "ProviderHealth"},
		{[]string{"provider-log", "--home", home, "--instance", "demo", "--after", "7", "--limit", "12"}, "ProviderObservations"},
		{[]string{"message", "--home", home, "--instance", "demo", "--text", "Please admit work", "--id", "m"}, "Message"},
		{[]string{"run", "--home", home, "--instance", "demo", "--prompt", "literal $(touch nope)", "--id", "t", "--authorizing-message", "m"}, "Admit"},
		{[]string{"board", "--home", home, "--instance", "demo", "--after", "task-7", "--limit", "12"}, "Board"},
		{[]string{"history", "--home", home, "--instance", "demo", "--task", "t", "--summary", "--after", "7", "--limit", "12"}, "History"},
		{[]string{"shutdown", "--home", home, "--instance", "demo", "--authorizing-message", "m"}, "Shutdown"},
	}
	for _, tt := range commands {
		var out bytes.Buffer
		if err := execute(context.Background(), tt.args, &out); err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(out.Bytes(), &body); err != nil || body["ok"] != true {
			t.Fatalf("bad JSON %s %v", out.String(), err)
		}
		r := <-requests
		if r.Method != tt.method {
			t.Fatal(r.Method)
		}
		if r.Method == "Board" || r.Method == "History" || r.Method == "ProviderObservations" {
			var params map[string]any
			if err := json.Unmarshal(r.Params, &params); err != nil {
				t.Fatal(err)
			}
			if params["limit"] != float64(12) {
				t.Fatal(params)
			}
			if r.Method == "Board" && params["after"] != "task-7" {
				t.Fatal(params)
			}
			if r.Method == "History" && (params["after"] != float64(7) || params["summary"] != true || params["id"] != "t") {
				t.Fatal(params)
			}
		}
		if r.Method == "Admit" {
			var p daemon.AdmitParams
			if err = json.Unmarshal(r.Params, &p); err != nil {
				t.Fatal(err)
			}
			if p.Content != "literal $(touch nope)" || p.Priority != 10 || p.Mode != "Local" || r.Key != "t" {
				t.Fatalf("bad admission %+v", p)
			}
		}
	}
}
func TestCLIInputLimits(t *testing.T) {
	file := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(file, []byte(strings.Repeat("x", (1<<20)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inputText("", file); err == nil {
		t.Fatal("oversized file accepted")
	}
	if _, err := inputText("", "missing"); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := inputText("x", "file"); exitCode(err) != 2 {
		t.Fatal(err)
	}
}
func TestCLIReadCredentialDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if _, err := readCredential(path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("created credential")
	}
}

func TestCLIDefaultContent(t *testing.T) {
	for _, verb := range []string{"default", "scenario"} {
		var out bytes.Buffer
		if err := execute(context.Background(), []string{"workflow", verb, "--json"}, &out); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(out.Bytes()) {
			t.Fatal("invalid default JSON")
		}
	}
}

func TestCLIStream(t *testing.T) {
	home := t.TempDir()
	layout, err := daemon.Paths(home, "s")
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
	server, _ := api.New(token, func(context.Context, api.Request) (any, error) { return nil, nil })
	server.SetEventStream(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") != "7" {
			t.Error("cursor not forwarded")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"sequence":8,"kind":"transition"}` + "\n"))
	})
	if err = server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() { server.Shutdown(context.Background()); <-done }()
	var out bytes.Buffer
	if err = execute(context.Background(), []string{"events", "--home", home, "--instance", "s", "--after", "7", "--follow"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"sequence":8`) {
		t.Fatal(out.String())
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/daemon"
)

func TestObservabilityCLIRefusals(t *testing.T) {
	if err := execute(context.Background(), []string{"alerts", "ack", "--id", "3"}, &bytes.Buffer{}); exitCode(err) != 1 {
		t.Fatal("uncited alerts ack must refuse with 1", err)
	}
	for _, args := range [][]string{{"alerts", "ack"}, {"alerts", "ack", "--authorizing-message", "m"}, {"alerts", "--limit", "0"}, {"alerts", "--after", "-1"}, {"alerts", "extra"}, {"metrics", "--window", "0s"}, {"metrics", "--window", "200h"}, {"host-metrics", "--window", "1h"}} {
		if err := execute(context.Background(), args, &bytes.Buffer{}); err == nil || exitCode(err) != 2 {
			t.Fatal(args, err)
		}
	}
	for _, verb := range []string{"alerts", "metrics", "host-metrics"} {
		if err := execute(context.Background(), []string{verb, "--home", t.TempDir()}, &bytes.Buffer{}); exitCode(err) != 3 {
			t.Fatal(verb, err)
		}
	}
}

func TestObservabilityCLIThinRPCs(t *testing.T) {
	home := shortHome(t) // the socket path must fit the 108-byte Unix limit under any TMPDIR
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
		params map[string]any
	}{
		{[]string{"alerts"}, "Alerts", map[string]any{"after": 0.0, "limit": 100.0, "unacknowledged": false}},
		{[]string{"alerts", "list", "--after", "7", "--limit", "20", "--unacknowledged"}, "Alerts", map[string]any{"after": 7.0, "limit": 20.0, "unacknowledged": true}},
		{[]string{"alerts", "ack", "--id", "3", "--authorizing-message", "m"}, "AlertAcknowledge", map[string]any{"id": 3.0, "authorizing_message": "m"}},
		{[]string{"metrics", "--window", "2h"}, "Metrics", map[string]any{"window_seconds": 7200.0}},
		{[]string{"metrics"}, "Metrics", map[string]any{"window_seconds": 86400.0}},
		{[]string{"host-metrics"}, "HostMetrics", map[string]any{}},
	} {
		var out bytes.Buffer
		if err := execute(context.Background(), append(tt.args, "--home", home, "--instance", "test"), &out); err != nil {
			t.Fatal(tt.args, err)
		}
		r := <-requests
		var params map[string]any
		if err := json.Unmarshal(r.Params, &params); err != nil || r.Method != tt.method || !reflect.DeepEqual(params, tt.params) {
			t.Fatalf("%v: %s %s %v", tt.args, r.Method, r.Params, err)
		}
	}
}

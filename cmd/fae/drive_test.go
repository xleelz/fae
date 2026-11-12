package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/daemon"
)

func TestDriveCLI(t *testing.T) {
	home := shortHome(t)
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

// FAE-CLI-001: each controller verb takes only its own flags. A flag that
// belongs to another verb is a usage error (2) that reaches no daemon, and
// each verb's help lists only its own flags.
func TestControllerVerbsTakeOnlyTheirOwnFlags(t *testing.T) {
	home := shortHome(t) // no daemon: a flag accepted would end in 1 or 3, not 2
	m := []string{"--authorizing-message", "m"}
	launcher := []string{"--note", "n", "--launcher-socket", "/run/fae-launcher.sock"}
	for _, args := range [][]string{
		append([]string{"maintenance", "stop", "--period", "1m"}, launcher...),
		append([]string{"maintenance", "stop", "--build-ref", "main"}, launcher...),
		{"maintenance", "start", "--note", "n"},
		{"maintenance", "start", "--overlap-wait", "1h"},
		{"maintenance", "status", "--launcher-socket", "/run/fae-launcher.sock"},
		{"maintenance", "status", "--max-wait", "1s"},
		{"drive", "stop", "--binary", "/bin/true"},
		{"drive", "status", "--period", "1m"},
		append([]string{"roll", "--binary", "/bin/true", "--fs-threshold", "0.5"}, launcher...),
		append([]string{"roll", "--binary", "/bin/true", "--socket", "/run/x.sock"}, launcher...),
		{"watchdog", "--binary", "/bin/true"},
		{"watchdog", "--launcher-socket", "/run/fae-launcher.sock"},
		{"runtime-launcher", "--socket", "/run/x.sock", "--note", "n"},
		{"runtime-launcher", "--socket", "/run/x.sock", "--period", "1m"},
	} {
		err := execute(context.Background(), append(append([]string{"--home", home, "--instance", "ctl"}, args...), m...), &bytes.Buffer{})
		if exitCode(err) != 2 || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Errorf("%v: exit %d %v, want usage 2", args, exitCode(err), err)
		}
	}
	for verb, want := range map[string][]string{
		"maintenance stop":   {"ack-overlap", "binary", "gated-ref", "launcher-socket", "max-wait", "note", "overlap-wait", "when-clear"},
		"maintenance start":  {"binary", "launcher-socket", "max-wait"},
		"maintenance status": {},
		"drive stop":         {},
		"roll":               {"ack-overlap", "binary", "build-ref", "gated-ref", "launcher-socket", "max-wait", "note", "overlap-wait", "when-clear"},
		"watchdog":           {"fs-threshold", "period", "rearm-budget", "rearm-window", "revoke-grant", "stall"},
		"runtime-launcher":   {"socket"},
	} {
		var out bytes.Buffer
		if err := execute(context.Background(), append(strings.Fields(verb), "--help", "--json"), &out); err != nil {
			t.Fatalf("%s --help: %v", verb, err)
		}
		var help struct {
			Flags []struct {
				Name string `json:"name"`
			} `json:"flags"`
		}
		if err := json.Unmarshal(out.Bytes(), &help); err != nil {
			t.Fatalf("%s --help: %v %s", verb, err, out.String())
		}
		got := []string{}
		for _, f := range help.Flags {
			if f.Name != "home" && f.Name != "instance" && f.Name != "json" && f.Name != "authorizing-message" {
				got = append(got, f.Name)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s --help lists %v, want %v", verb, got, want)
		}
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatal("refused controller verbs wrote instance files", entries, err)
	}
}

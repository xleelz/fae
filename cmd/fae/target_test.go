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

func TestTargetCLI(t *testing.T) {
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
	file := filepath.Join(home, "target.json")
	if err = os.WriteFile(file, []byte(`{"schema_version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		args   []string
		method string
	}{{[]string{"template", "adapter-retire", "--id", "x", "--reason", "replaced"}, "AdapterRetire"}, {[]string{"template", "adapter-put", "--id", "x", "--content", file}, "AdapterPut"}, {[]string{"template", "adapter-get", "--id", "x", "--revision", "1"}, "AdapterGet"}, {[]string{"template", "adapter-list"}, "AdapterList"}, {[]string{"template", "target-config-put", "--id", "x", "--content", file}, "TargetConfigPut"}, {[]string{"template", "target-config-get", "--id", "x", "--revision", "2"}, "TargetConfigGet"}, {[]string{"template", "target-config-list"}, "TargetConfigList"}, {[]string{"template", "target-config-retire", "--id", "x", "--reason", "replaced"}, "TargetConfigRetire"}, {[]string{"run", "--prompt", "work", "--target-config", "x", "--target-revision", "2"}, "Admit"}} {
		args := append(tt.args, "--home", home, "--instance", "test", "--authorizing-message", "m")
		if err = execute(context.Background(), args, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		r := <-requests
		if r.Method != tt.method {
			t.Fatal(r)
		}
		if tt.method == "Admit" {
			var p daemon.AdmitParams
			if err = json.Unmarshal(r.Params, &p); err != nil || p.TargetConfig != "x" || p.TargetRevision != 2 {
				t.Fatal(p, err)
			}
		}
	}
	for _, args := range [][]string{{"template", "adapter-retire", "--id", "x"}, {"template", "adapter-put"}, {"template", "adapter-get", "--id", "x", "--revision", "-1"}, {"template", "target-config-put"}, {"template", "target-config-get", "--revision", "-1"}, {"template", "target-config-retire", "--id", "x"}} {
		if err = execute(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatal("missing args accepted")
		}
	}
}

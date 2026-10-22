package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/daemon"
)

func TestCensusCLI(t *testing.T) {
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
	calls := make(chan api.Request, 10)
	server, err := api.New(token, func(_ context.Context, r api.Request) (any, error) {
		calls <- r
		var p daemon.CensusApplyParams
		json.Unmarshal(r.Params, &p)
		if p.RowID == "b" {
			return nil, errors.New("stale census snapshot")
		}
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
	a := daemon.CensusArtifact{Format: 1, Instance: "test"}
	for _, id := range []string{"a", "b", "c"} {
		a.Rows = append(a.Rows, daemon.CensusRow{ID: id, State: "Escalated", Version: 1, SnapshotDigest: strings.Repeat("a", 64), Classification: "obsolete", Reason: "obsolete ruling", Evidence: "ruling/123"})
	}
	a.Digest = daemon.CensusDigest(a)
	file := filepath.Join(home, "census.json")
	raw, _ := json.Marshal(a)
	os.WriteFile(file, raw, 0600)
	var out bytes.Buffer
	args := []string{"census", "apply", "--file", file, "--gap", "0s", "--home", home, "--instance", "test", "--authorizing-message", "batch"}
	if err = execute(context.Background(), args, &out); err == nil {
		t.Fatal("failed row accepted")
	}
	var result struct {
		Completed []string `json:"completed"`
		Failed    string   `json:"failed"`
	}
	if err = json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err, out.String())
	}
	if len(result.Completed) != 1 || result.Completed[0] != "a" || result.Failed != "b" || len(calls) != 2 {
		t.Fatal(result, len(calls))
	}
	for len(calls) > 0 {
		r := <-calls
		var p daemon.CensusApplyParams
		json.Unmarshal(r.Params, &p)
		if r.Method != "CensusApply" || p.AuthorizingMessage != "batch" {
			t.Fatal(r)
		}
	}
	a.Digest = "tampered"
	raw, _ = json.Marshal(a)
	os.WriteFile(file, raw, 0600)
	if execute(context.Background(), args, &bytes.Buffer{}) == nil || len(calls) != 0 {
		t.Fatal("invalid artifact reached server")
	}
	for _, args := range [][]string{{"census", "apply"}, {"census", "apply", "--gap", "-1s"}, {"census", "apply", "--gap", "2m"}} {
		if execute(context.Background(), args, &bytes.Buffer{}) == nil {
			t.Fatal(args)
		}
	}
}

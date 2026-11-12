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

func TestHerdrSnapshotOfflineExitsZero(t *testing.T) {
	home := shortHome(t)
	var out bytes.Buffer
	err := execute(context.Background(), []string{"herdr", "snapshot", "--home", home, "--instance", "offline-inst", "--json"}, &out)
	if err != nil {
		t.Fatalf("herdr snapshot offline returned error: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(out.Bytes(), &snap); err != nil {
		t.Fatalf("invalid snapshot JSON: %s (%v)", out.String(), err)
	}
	if snap["instance"] != "offline-inst" || snap["reachable"] != false || snap["error"] == nil {
		t.Fatalf("unexpected offline snapshot: %+v", snap)
	}
}

func TestHerdrSnapshotWatchAndLabelSync(t *testing.T) {
	home := shortHome(t)
	layout, err := daemon.Paths(home, "herdr-demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.Run, 0700); err != nil {
		t.Fatal(err)
	}
	credential, err := api.Credential(layout.Credential)
	if err != nil {
		t.Fatal(err)
	}

	server, err := api.New(credential, func(_ context.Context, r api.Request) (any, error) {
		switch r.Method {
		case "Status":
			return map[string]any{
				"active":   1,
				"draining": false,
				"counts":   map[string]int{"Running": 1},
				"tasks": []map[string]any{
					{"id": "T-142", "state": "Running", "molecule_id": "mol-9"},
				},
			}, nil
		case "Board":
			return map[string]any{
				"items": []map[string]any{
					{"id": "T-142", "state": "Running", "molecule_id": "mol-9"},
				},
			}, nil
		case "Topology":
			return map[string]any{"molecules": []any{}}, nil
		case "Inbox":
			return map[string]any{"items": []any{}}, nil
		case "ProviderHealth":
			return map[string]any{"series": map[string]any{}}, nil
		case "DispatchPauseList":
			return map[string]any{"pauses": []any{}}, nil
		case "Leases":
			return map[string]any{"role_slots": []any{}}, nil
		case "Panes":
			return map[string]any{
				"sessions": []map[string]any{
					{
						"session": "sess-142",
						"role":    "coder",
						"task":    "T-142",
						"stage":   "implement",
						"binding": "claude/claude-sonnet-4-6",
					},
				},
			}, nil
		case "HostMetrics":
			return map[string]any{"memory_available_bytes": 16 << 30}, nil
		case "Alerts":
			return map[string]any{"alerts": []any{}}, nil
		case "WorkflowList":
			return map[string]any{"workflows": []any{}}, nil
		case "Events":
			return map[string]any{"events": []any{}}, nil
		default:
			return map[string]any{"ok": true}, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() {
		_ = server.Shutdown(context.Background())
		<-done
	}()

	// 1. Test fae herdr snapshot --json
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"herdr", "snapshot", "--home", home, "--instance", "herdr-demo", "--json"}, &out); err != nil {
		t.Fatalf("herdr snapshot failed: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(out.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v\n%s", err, out.String())
	}
	if snap["reachable"] != true || snap["instance"] != "herdr-demo" {
		t.Fatalf("unexpected snapshot header: %+v", snap)
	}
	for _, key := range []string{
		"status", "board", "molecules", "inbox", "pools", "pauses",
		"leases", "panes", "host", "symphony", "alerts", "workflows", "events",
	} {
		if _, ok := snap[key]; !ok {
			t.Fatalf("snapshot missing required key %q", key)
		}
	}

	// 2. Test fae herdr watch --once --out <tmpfile>
	tmpDir := t.TempDir()
	outFile := filepath.Join(tmpDir, "data", "fae", "snapshot.json")
	out.Reset()
	if err := execute(context.Background(), []string{
		"herdr", "watch", "--once", "--out", outFile,
		"--home", home, "--instance", "herdr-demo", "--json",
	}, &out); err != nil {
		t.Fatalf("herdr watch --once failed: %v", err)
	}
	writtenRaw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read watched snapshot file: %v", err)
	}
	var watchedSnap map[string]any
	if err := json.Unmarshal(writtenRaw, &watchedSnap); err != nil || watchedSnap["reachable"] != true {
		t.Fatalf("invalid watched snapshot file: %s (%v)", string(writtenRaw), err)
	}

	// 3. Test fae herdr label-sync with fake hc binary
	labelLog := filepath.Join(tmpDir, "labels.log")
	fakeHC := filepath.Join(tmpDir, "fake-hc.sh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"ls\" ]; then\n" +
		"  printf '{\"terminals\":[{\"address\":\"t7\",\"name\":\"sess-142\",\"state\":\"live\",\"cwd\":\"/tmp/worktrees/T-142\",\"labels\":{}}]}\\n'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"label\" ]; then\n" +
		"  printf '%s\\n' \"$*\" >> " + labelLog + "\n" +
		"  printf '{\"ok\":true}\\n'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(fakeHC, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := execute(context.Background(), []string{
		"herdr", "label-sync", "--hc-bin", fakeHC,
		"--home", home, "--instance", "herdr-demo", "--json",
	}, &out); err != nil {
		t.Fatalf("herdr label-sync failed: %v", err)
	}
	var syncRes struct {
		Synced    int  `json:"synced"`
		Reachable bool `json:"reachable"`
	}
	if err := json.Unmarshal(out.Bytes(), &syncRes); err != nil || syncRes.Synced != 1 || !syncRes.Reachable {
		t.Fatalf("unexpected label-sync result: %s (%v)", out.String(), err)
	}
	loggedBytes, err := os.ReadFile(labelLog)
	if err != nil {
		t.Fatalf("missing label log: %v", err)
	}
	logged := string(loggedBytes)
	for _, wantSub := range []string{
		"label t7",
		"fae.instance=herdr-demo",
		"fae.task=T-142",
		"fae.stage=implement",
		"fae.state=Running",
		"fae.molecule=mol-9",
		"herdr.space=herdr-demo",
		"herdr.tab=mol-9",
	} {
		if !strings.Contains(logged, wantSub) {
			t.Fatalf("label command %q missing %q", logged, wantSub)
		}
	}
}

// A reachable daemon can still reject individual reads. Preserve those failures
// so UI clients do not mistake an empty replacement object for healthy data.
func TestHerdrSnapshotReportsPartialReadFailures(t *testing.T) {
	home := shortHome(t)
	layout, err := daemon.Paths(home, "partial")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.Run, 0700); err != nil {
		t.Fatal(err)
	}
	credential, err := api.Credential(layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(credential, func(_ context.Context, request api.Request) (any, error) {
		switch request.Method {
		case "Status":
			return map[string]any{"pid": 1, "snapshot": map[string]any{"active": 0}}, nil
		case "Board":
			return nil, errors.New("board read deadline exceeded")
		case "Alerts":
			return nil, errors.New("alerts temporarily unavailable")
		default:
			return map[string]any{}, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() { _ = server.Shutdown(context.Background()); <-done }()
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"herdr", "snapshot", "--home", home, "--instance", "partial", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Reachable bool              `json:"reachable"`
		Partial   bool              `json:"partial"`
		Errors    map[string]string `json:"errors"`
		Board     any               `json:"board"`
		Status    map[string]any    `json:"status"`
	}
	if err := json.Unmarshal(out.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Reachable || !snapshot.Partial || snapshot.Board != nil || len(snapshot.Errors) != 2 || snapshot.Errors["Board"] != "board read deadline exceeded" || snapshot.Errors["Alerts"] != "alerts temporarily unavailable" || snapshot.Status["pid"] != float64(1) {
		t.Fatalf("partial reads concealed or successful status lost: %s", out.String())
	}
}

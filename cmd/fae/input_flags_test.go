package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/xleelz/fae/daemon"
	"os"
	"path/filepath"
	"testing"
)

func TestTypedInputAndDetachedSubmissionFlags(t *testing.T) {
	home, requests := recordingDaemon(t, "inputs", map[string]any{"id": "root.detached.1"})
	file := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(file, []byte("enabled: true\ncomponents:\n  - a\n  - b\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := execute(context.Background(), []string{"run", "--prompt", "refine", "--parent", "root", "--from-parent", "contract=contract", "--input", "profile=@" + file, "--input", "n=3", "--input", "s=abc", "--max-sessions", "2", "--home", home, "--instance", "inputs", "--authorizing-message", "m"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	r := <-requests
	var p daemon.AdmitParams
	if json.Unmarshal(r.Params, &p) != nil || p.ID != "" || p.ParentTask != "root" || p.MaxSessions != 2 || p.Inputs["n"] != float64(3) || p.Inputs["s"] != "abc" || p.Inputs["profile"].(map[string]any)["enabled"] != true {
		t.Fatal("typed CLI fields", string(r.Params))
	}
}

func TestDetachedSubmissionOmitsUnspecifiedMaxSessions(t *testing.T) {
	for _, args := range [][]string{{"run", "--prompt", "refine", "--parent", "root", "--from-parent", "a=b"}, {"backlog", "admit", "--parent", "root"}} {
		t.Run(args[0], func(t *testing.T) {
			home, requests := recordingDaemon(t, "omit", map[string]any{"id": "root.detached.1"})
			if args[0] == "backlog" {
				file := filepath.Join(t.TempDir(), "item.json")
				if err := os.WriteFile(file, []byte(`{"items":[{"id":"root.detached.1","workflow_id":"child"}]}`), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--file", file)
			}
			args = append(args, "--home", home, "--instance", "omit", "--authorizing-message", "m")
			if err := execute(context.Background(), args, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			r := <-requests
			var p map[string]any
			_ = json.Unmarshal(r.Params, &p)
			if args[0] == "backlog" {
				p = p["items"].([]any)[0].(map[string]any)
			}
			if _, ok := p["max_sessions"]; ok {
				t.Fatal("implicit cap sent on child admission", string(r.Params))
			}
		})
	}
}

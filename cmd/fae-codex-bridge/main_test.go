package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	r "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

func TestBridgeUsageSubprocess(t *testing.T) {
	if os.Getenv("BRIDGE_USAGE_TEST") == "1" {
		os.Args = []string{"fae-codex-bridge"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBridgeUsageSubprocess$")
	cmd.Env = append(os.Environ(), "BRIDGE_USAGE_TEST=1")
	if e := cmd.Run(); e == nil {
		t.Fatal("missing config succeeded")
	}
}

// An absent --files pins nothing: the brief's task_scope block is the scope
// (strings.Split("", ",") would otherwise pin the unsafe path "").
func TestBridgeTakesScopeFromBriefSubprocess(t *testing.T) {
	if os.Getenv("BRIDGE_SCOPE_TEST") == "1" {
		os.Args = []string{"fae-codex-bridge", "--codex", os.Getenv("BRIDGE_FAKE"), "--config", os.Getenv("BRIDGE_CONFIG"), "--timeout", "10s"}
		main()
		return
	}
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	git := exec.Command("git", "init", "-q", root)
	git.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, e := git.CombinedOutput(); e != nil {
		t.Fatal(e, string(out))
	}
	if e = os.WriteFile(filepath.Join(root, "answer.go"), []byte("original"), 0600); e != nil {
		t.Fatal(e)
	}
	brief := json.RawMessage(`[{"kind":"stage","stage":"implement"},{"kind":"task_scope","files":["answer.go"],"test_file":""},{"kind":"kernel_contract","output_schema":{"type":"object","properties":{"accepted":{"type":"boolean"}},"required":["accepted"],"additionalProperties":false}}]`)
	l := r.BridgeConfig{Protocol: "fae-agent-v1", Launch: r.Launch{DispatchID: "d", SessionID: "s", TaskID: "t", Worktree: root, Binding: workflow.Binding{Provider: "codex", Model: "configured-model"}, Profile: workflow.Profile{Allowed: []string{"handoff", "edit"}}, Brief: brief}, Handoff: filepath.Join(root, "handoff.json"), LogDirectory: root}
	raw, _ := json.Marshal(l)
	config := filepath.Join(root, "config.json")
	if e = os.WriteFile(config, raw, 0600); e != nil {
		t.Fatal(e)
	}
	fake := filepath.Join(root, "fake-codex")
	if e = os.WriteFile(fake, []byte(`#!/bin/sh
out=''
while [ "$#" -gt 0 ]; do
 if [ "$1" = '--output-last-message' ]; then shift; out="$1"; fi
 shift
done
cat >/dev/null
printf '%s' '{"output":{"accepted":true},"edits":[{"path":"answer.go","content":"package answer\n","replacements":[]}]}' > "$out"
`), 0700); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBridgeTakesScopeFromBriefSubprocess$")
	cmd.Env = append(os.Environ(), "BRIDGE_SCOPE_TEST=1", "BRIDGE_FAKE="+fake, "BRIDGE_CONFIG="+config)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatal(e, string(out))
	}
	if data, _ := os.ReadFile(filepath.Join(root, "answer.go")); string(data) != "package answer\n" {
		t.Fatal("block-scoped edit not applied", string(data))
	}
}

// FAE-WF-034, FAE-RT-004: the bridge exits with the protocol's refusal status
// and leaves a refusal record for a refused provider answer (here a schema
// failure) and for a turn refused before the provider ran (here a task_scope
// whose test file is outside its files). A provider that exits before
// writing anything (before acknowledging the prompt) is a recorded
// provider-not-started (FAE-RT-004), and so is one still silent when its
// --start-timeout (the agent-start bound) runs out; one that failed after its
// first event is a recorded provider-failed: no answer, and nothing written
// (FAE-WF-034).
func TestBridgeRefusalExitStatusSubprocess(t *testing.T) {
	if os.Getenv("BRIDGE_REFUSAL_TEST") == "1" {
		os.Args = []string{"fae-codex-bridge", "--codex", os.Getenv("BRIDGE_FAKE"), "--config", os.Getenv("BRIDGE_CONFIG"), "--timeout", "10s"}
		if bound := os.Getenv("BRIDGE_START_TIMEOUT"); bound != "" {
			os.Args = append(os.Args, "--start-timeout", bound)
		}
		main()
		return
	}
	for _, tc := range []struct {
		name, testFile, class string
		fail, ran             bool
		exit                  int
		hang                  bool
	}{
		{"schema", "", r.RefusedOutput, false, true, r.BridgeRefusalExit, false},
		{"before the provider", "answer_test.go", r.RefusedBeforeProvider, false, false, r.BridgeRefusalExit, false},
		{"provider failed", "", "provider-failed", true, true, r.BridgeRefusalExit, false},
		{"provider never acknowledged the prompt", "", r.ProviderNotStarted, false, true, r.BridgeRefusalExit, false},
		{"provider silent past the start bound", "", r.ProviderNotStarted, false, true, r.BridgeRefusalExit, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, e := filepath.EvalSymlinks(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			git := exec.Command("git", "init", "-q", root)
			git.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
			if out, e := git.CombinedOutput(); e != nil {
				t.Fatal(e, string(out))
			}
			if e = os.WriteFile(filepath.Join(root, "answer.go"), []byte("original"), 0600); e != nil {
				t.Fatal(e)
			}
			scope, _ := json.Marshal(map[string]any{"kind": "task_scope", "files": []string{"answer.go"}, "test_file": tc.testFile})
			brief := json.RawMessage(`[{"kind":"stage","stage":"implement"},` + string(scope) + `,{"kind":"kernel_contract","output_schema":{"type":"object","properties":{"accepted":{"type":"boolean"}},"required":["accepted"],"additionalProperties":false}}]`)
			l := r.BridgeConfig{Protocol: "fae-agent-v1", Launch: r.Launch{DispatchID: "d", SessionID: "s", TaskID: "t", Worktree: root, Binding: workflow.Binding{Provider: "codex", Model: "configured-model"}, Profile: workflow.Profile{Allowed: []string{"handoff", "edit"}}, Brief: brief}, Handoff: filepath.Join(root, "handoff.json"), LogDirectory: root}
			raw, _ := json.Marshal(l)
			config := filepath.Join(root, "config.json")
			if e = os.WriteFile(config, raw, 0600); e != nil {
				t.Fatal(e)
			}
			fake := filepath.Join(t.TempDir(), "fake-codex")
			if e = os.WriteFile(fake, []byte(`#!/bin/sh
out=''
while [ "$#" -gt 0 ]; do
 if [ "$1" = '--output-last-message' ]; then shift; out="$1"; fi
 shift
done
cat >/dev/null
touch "$0.ran"
[ -e "$0.hang" ] && exec sleep 20
[ -e "$0.silent" ] && exit 3
[ -e "$0.fail" ] && { printf '%s\n' '{"type":"thread.started","thread_id":"t"}'; exit 3; }
printf '%s' '{"output":{"accepted":"yes"},"edits":[]}' > "$out"
`), 0700); e != nil {
				t.Fatal(e)
			}
			if tc.fail {
				if e = os.WriteFile(fake+".fail", nil, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if tc.class == r.ProviderNotStarted {
				if e = os.WriteFile(fake+".silent", nil, 0600); e != nil {
					t.Fatal(e)
				}
			}
			bound := ""
			if tc.hang {
				if e = os.WriteFile(fake+".hang", nil, 0600); e != nil {
					t.Fatal(e)
				}
				bound = "300ms"
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestBridgeRefusalExitStatusSubprocess$")
			cmd.Env = append(os.Environ(), "BRIDGE_REFUSAL_TEST=1", "BRIDGE_FAKE="+fake, "BRIDGE_CONFIG="+config, "BRIDGE_START_TIMEOUT="+bound)
			out, e := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != tc.exit {
				t.Fatalf("exit %v, want %d: %s", e, tc.exit, out)
			}
			var record struct{ Class, Detail string }
			raw, e = os.ReadFile(filepath.Join(root, r.BridgeRefusalFile))
			if tc.class == "" {
				if !os.IsNotExist(e) {
					t.Fatal("untyped failure left a refusal record", string(raw))
				}
			} else if e != nil || json.Unmarshal(raw, &record) != nil || record.Class != tc.class || record.Detail == "" {
				t.Fatalf("refusal record %s %v", raw, e)
			}
			if tc.hang && !strings.Contains(record.Detail, "within the start bound of 300ms") {
				t.Fatalf("refusal detail %q does not name the start bound", record.Detail)
			}
			if _, e = os.Stat(fake + ".ran"); (e == nil) != tc.ran {
				t.Fatalf("provider ran: %v, want %v", e == nil, tc.ran)
			}
			if data, _ := os.ReadFile(filepath.Join(root, "answer.go")); string(data) != "original" {
				t.Fatal("refused turn changed the task", string(data))
			}
		})
	}
}

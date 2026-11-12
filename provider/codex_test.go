package provider

import (
	"context"
	"encoding/json"
	r "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitRepository makes dir a Git repository, as every task worktree is, so the
// bridge can list it; tracked names existing files to add to the index.
func gitRepository(t *testing.T, dir string, tracked ...string) {
	t.Helper()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatal(e, string(out))
		}
	}
	git("init", "-q")
	if len(tracked) > 0 {
		git(append([]string{"add", "--"}, tracked...)...)
	}
}

// decodeResponse decodes a provider answer the way a test states it.
func decodeResponse(t *testing.T, raw string) Response {
	t.Helper()
	var response Response
	if e := json.Unmarshal([]byte(raw), &response); e != nil {
		t.Fatal(e)
	}
	return response
}

func TestEditPolicy(t *testing.T) {
	root := t.TempDir()
	l := r.BridgeConfig{Launch: r.Launch{Worktree: root, Profile: workflow.Profile{Allowed: []string{"handoff", "edit"}}}}
	c := Config{Files: []string{"answer.go", "answer_test.go"}, TestFile: "answer_test.go"}
	for _, tc := range []struct {
		stage, path string
		ok          bool
	}{{"test-first", "answer_test.go", true}, {"test-first", "answer.go", false}, {"implement", "answer.go", true}, {"implement", "answer_test.go", false}, {"review", "answer.go", false}, {"implement", "../escape", false}} {
		err := validateEdits(c, l, tc.stage, decodeResponse(t, `{"edits":[{"path":"`+tc.path+`","content":"x"}]}`))
		if (err == nil) != tc.ok {
			t.Fatalf("%+v %v", tc, err)
		}
	}
	l.Profile.Denied = []string{"edit"}
	if validateEdits(c, l, "implement", decodeResponse(t, `{"edits":[{"path":"answer.go","content":""}]}`)) == nil {
		t.Fatal("denied edit accepted")
	}
	l.Profile.Build = true
	if validateEdits(c, l, "spec", Response{}) == nil {
		t.Fatal("host profile accepted")
	}
}
func TestPathAndBoundaries(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"../escape", "/absolute", "a/../b", ".git/config", ""} {
		if _, e := safePath(root, path); e == nil {
			t.Fatal(path)
		}
	}
	if e := os.Symlink(t.TempDir(), filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	if _, e := safePath(root, "link/file"); e == nil {
		t.Fatal("symlink accepted")
	}
	p := filepath.Join(root, "big")
	os.WriteFile(p, []byte(strings.Repeat("x", (1<<20)+1)), 0600)
	if _, e := regularRead(p, false); e == nil {
		t.Fatal("oversized input accepted")
	}
}
func TestBridgeSubprocess(t *testing.T) {
	root := t.TempDir()
	physical, e := filepath.EvalSymlinks(root)
	if e != nil {
		t.Fatal(e)
	}
	root = physical
	gitRepository(t, root)
	schema := json.RawMessage(`{"type":"object","properties":{"accepted":{"type":"boolean"}},"required":["accepted"],"additionalProperties":false}`)
	brief, _ := json.Marshal([]any{map[string]any{"kind": "stage", "stage": "implement"}, map[string]any{"kind": "kernel_contract", "output_schema": schema}})
	l := r.BridgeConfig{Protocol: "fae-agent-v1", Launch: r.Launch{DispatchID: "d", SessionID: "s", TaskID: "t", Worktree: root, Binding: workflow.Binding{Provider: "codex", Model: "configured-model"}, Profile: workflow.Profile{Allowed: []string{"handoff", "edit"}}, Brief: brief}, Handoff: filepath.Join(root, "handoff.json"), LogDirectory: root}
	raw, _ := json.Marshal(l)
	config := filepath.Join(root, "config.json")
	os.WriteFile(config, raw, 0600)
	fake := filepath.Join(root, "fake-codex")
	script := `#!/bin/sh
out=''
while [ "$#" -gt 0 ]; do
 if [ "$1" = '--output-last-message' ]; then shift; out="$1"; fi
 shift
done
cat >/dev/null
printf '%s' '{"output":{"accepted":true},"edits":[{"path":"answer.go","content":"package sample\n"}]}' > "$out"
`
	os.WriteFile(fake, []byte(script), 0700)
	if e := Run(context.Background(), Config{Binary: fake, Files: []string{"answer.go"}, Timeout: time.Second}, config); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(root, "answer.go"))
	if e != nil || string(data) != "package sample\n" {
		t.Fatal(string(data), e)
	}
	data, e = os.ReadFile(l.Handoff)
	if e != nil {
		t.Fatal(e)
	}
	var h r.Handoff
	if json.Unmarshal(data, &h) != nil || h.SessionID != "s" || string(h.Output) != "{\"accepted\":true}" {
		t.Fatal(string(data))
	}
	// Existing handoffs are never overwritten by a fresh role response.
	if e := Run(context.Background(), Config{Binary: fake, Files: []string{"answer.go"}, Timeout: time.Second}, config); e == nil {
		t.Fatal("stale handoff overwritten")
	}
}

func providerFixture(t *testing.T, extra string) (Config, string, r.BridgeConfig) {
	t.Helper()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	gitRepository(t, root)
	brief := json.RawMessage(`[{"kind":"stage","stage":"implement"},{"kind":"kernel_contract","output_schema":{"type":"object","properties":{"accepted":{"type":"boolean"}},"required":["accepted"],"additionalProperties":false}}]`)
	l := r.BridgeConfig{Protocol: "fae-agent-v1", Launch: r.Launch{DispatchID: "d", SessionID: "s", TaskID: "t", Worktree: root, Binding: workflow.Binding{Provider: "codex", Model: "configured-model"}, Profile: workflow.Profile{Allowed: []string{"handoff", "edit"}}, Brief: brief}, Handoff: filepath.Join(root, "handoff.json"), LogDirectory: root}
	raw, _ := json.Marshal(l)
	config := filepath.Join(root, "config.json")
	if e = os.WriteFile(config, raw, 0600); e != nil {
		t.Fatal(e)
	}
	fake := filepath.Join(root, "provider")
	script := `#!/bin/sh
out=''
while [ "$#" -gt 0 ]; do
 if [ "$1" = '--output-last-message' ]; then shift; out="$1"; fi
 shift
done
cat >/dev/null
` + extra + `
printf '%s' '{"output":{"accepted":true},"edits":[{"path":"answer.go","content":"new content"}]}' > "$out"
`
	if e = os.WriteFile(fake, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "answer.go"), []byte("original"), 0600); e != nil {
		t.Fatal(e)
	}
	return Config{Binary: fake, Files: []string{"answer.go"}, Timeout: 5 * time.Second}, config, l
}
func TestExistingHandoffRefusesBeforeExecutionOrEdits(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			c, path, l := providerFixture(t, `touch "$out.executed"`)
			switch kind {
			case "file":
				os.WriteFile(l.Handoff, []byte("old"), 0600)
			case "symlink":
				os.Symlink("missing-target", l.Handoff)
			case "directory":
				os.Mkdir(l.Handoff, 0700)
			}
			if e := Run(context.Background(), c, path); e == nil || !strings.Contains(e.Error(), "handoff already exists") {
				t.Fatal(e)
			}
			providers, _ := filepath.Glob(filepath.Join(l.LogDirectory, "provider-*"))
			if len(providers) != 0 {
				t.Fatal("provider launched despite existing handoff")
			}
			data, _ := os.ReadFile(filepath.Join(l.Worktree, "answer.go"))
			if string(data) != "original" {
				t.Fatal("task file changed", string(data))
			}
		})
	}
}
func TestConcurrentHandoffRefusesBeforeEdits(t *testing.T) {
	c, path, l := providerFixture(t, `printf 'concurrent' > "$(dirname "$(dirname "$out")")/handoff.json"`)
	if e := Run(context.Background(), c, path); e == nil {
		t.Fatal("concurrent handoff accepted")
	}
	data, _ := os.ReadFile(filepath.Join(l.Worktree, "answer.go"))
	if string(data) != "original" {
		t.Fatal("task file changed", string(data))
	}
	handoff, _ := os.ReadFile(l.Handoff)
	if string(handoff) != "concurrent" {
		t.Fatal("concurrent handoff modified", string(handoff))
	}
}
func TestProviderLogsBoundedAndFullyDrained(t *testing.T) {
	c, path, l := providerFixture(t, `head -c 2097152 /dev/zero
head -c 2097152 /dev/zero >&2`)
	if e := Run(context.Background(), c, path); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"events.jsonl", "stderr.log"} {
		paths, _ := filepath.Glob(filepath.Join(l.LogDirectory, "provider-*", name))
		if len(paths) != 1 {
			t.Fatal(paths)
		}
		st, e := os.Stat(paths[0])
		if e != nil || st.Size() != maxProviderLogBytes || st.Mode().Perm() != 0600 {
			t.Fatal(st, e)
		}
	}
	if _, e := os.Stat(l.Handoff); e != nil {
		t.Fatal("logging cap prevented successful handoff", e)
	}
}
func TestBoundedLogChunkBoundaries(t *testing.T) {
	var buf strings.Builder
	w := boundedLog{destination: &buf, remaining: 5}
	for _, s := range []string{"abc", "defgh", "tail"} {
		if n, e := w.Write([]byte(s)); n != len(s) || e != nil {
			t.Fatal(n, e)
		}
	}
	if buf.String() != "abcde" || w.remaining != 0 {
		t.Fatal(buf.String(), w.remaining)
	}
}

func TestInvalidProviderSchemaCannotEdit(t *testing.T) {
	c, path, l := providerFixture(t, "")
	script, e := os.ReadFile(c.Binary)
	if e != nil {
		t.Fatal(e)
	}
	script = []byte(strings.Replace(string(script), `"accepted":true`, `"accepted":"wrong type"`, 1))
	if e = os.WriteFile(c.Binary, script, 0700); e != nil {
		t.Fatal(e)
	}
	if e = Run(context.Background(), c, path); e == nil {
		t.Fatal("schema-invalid provider accepted")
	}
	data, _ := os.ReadFile(filepath.Join(l.Worktree, "answer.go"))
	if string(data) != "original" {
		t.Fatal("invalid response changed task", string(data))
	}
	if _, e = os.Lstat(l.Handoff); !os.IsNotExist(e) {
		t.Fatal("invalid response published handoff", e)
	}
}

func TestPinnedTaskScopes(t *testing.T) {
	for _, tc := range []struct {
		name, scopes string
		ok           bool
	}{
		{"matching", `{"t":{"files":["answer.go","test.go"],"test_file":"test.go"}}`, true},
		{"foreign task", `{"other":{"files":["answer.go"],"test_file":"test.go"}}`, false},
		{"wrong scope", `{"t":{"files":["different.go"],"test_file":"test.go"}}`, false},
		{"unknown field", `{"t":{"files":["answer.go"],"test_file":"test.go","extra":true}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, path, l := providerFixture(t, "")
			c.TaskScopesJSON = tc.scopes
			e := Run(context.Background(), c, path)
			if (e == nil) != tc.ok {
				t.Fatal(e)
			}
			if !tc.ok {
				data, _ := os.ReadFile(filepath.Join(l.Worktree, "answer.go"))
				if string(data) != "original" {
					t.Fatal("scope violation changed task")
				}
			}
		})
	}
}

func TestProviderLoopRequiresPersistentReadOnlyContext(t *testing.T) {
	for _, tc := range []string{"no-root", "fresh", "unscoped", "edit", "commands"} {
		t.Run(tc, func(t *testing.T) {
			c, path, l := providerFixture(t, "")
			l.WorkflowID = "fae-loop:demo"
			l.WorkflowRevision = "v1"
			l.Role = "oracle"
			l.SessionPolicy = "task-bound"
			l.Profile = workflow.Profile{Allowed: []string{"read", "handoff"}}
			c.SessionRoot = filepath.Join(l.Worktree, "contexts")
			switch tc {
			case "no-root":
				c.SessionRoot = ""
			case "fresh":
				l.FreshContext = true
			case "unscoped":
				l.SessionPolicy = ""
			case "edit":
				l.Profile.Allowed = append(l.Profile.Allowed, "edit")
			case "commands":
				l.Profile.Test = true
			}
			raw, _ := json.Marshal(l)
			os.WriteFile(path, raw, 0600)
			if e := Run(context.Background(), c, path); e == nil || !strings.Contains(e.Error(), "loop") {
				t.Fatal(e)
			}
		})
	}
}

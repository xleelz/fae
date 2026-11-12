package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	r "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// turnFixture is providerFixture with the given brief blocks between the
// stage block and the kernel contract, and a fake Codex that saves its prompt
// and output schema in its working directory and answers with answer.
func turnFixture(t *testing.T, stage string, blocks []any, answer string) (Config, string, r.BridgeConfig) {
	t.Helper()
	c, config, l := providerFixture(t, "")
	contract := map[string]any{"kind": "kernel_contract", "output_schema": json.RawMessage(`{"type":"object","properties":{"accepted":{"type":"boolean"}},"required":["accepted"],"additionalProperties":false}`)}
	brief := append(append([]any{map[string]any{"kind": "stage", "stage": stage}}, blocks...), contract)
	l.Brief, _ = json.Marshal(brief)
	writeLaunch(t, config, l)
	script := `#!/bin/sh
out=''
schema=''
while [ "$#" -gt 0 ]; do
 case "$1" in --output-last-message) shift; out="$1";; --output-schema) shift; schema="$1";; esac
 shift
done
cat > prompt.json
cp "$schema" schema.json
cat > "$out" <<'FAE_ANSWER'
` + answer + `
FAE_ANSWER
`
	if e := os.WriteFile(c.Binary, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	return c, config, l
}

func writeLaunch(t *testing.T, config string, l r.BridgeConfig) {
	t.Helper()
	raw, _ := json.Marshal(l)
	if e := os.WriteFile(config, raw, 0600); e != nil {
		t.Fatal(e)
	}
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, []byte(content), 0600); e != nil {
		t.Fatal(e)
	}
}

func readFile(t *testing.T, root, name string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(root, name))
	return string(data)
}

// launched reports whether the bridge started a provider for this turn.
func launched(l r.BridgeConfig) bool {
	dirs, _ := filepath.Glob(filepath.Join(l.LogDirectory, "provider-*"))
	return len(dirs) > 0
}

// promptFiles returns the files block of the turn's saved prompt.
func promptFiles(t *testing.T, l r.BridgeConfig) map[string]string {
	t.Helper()
	var block struct{ Files map[string]string }
	if e := json.Unmarshal(promptBlocks(t, []byte(providerFile(t, l.LogDirectory, "prompt.json")))["files"], &block); e != nil {
		t.Fatal(e)
	}
	return block.Files
}

func taskScope(files []string, testFile string) map[string]any {
	return map[string]any{"kind": "task_scope", "files": files, "test_file": testFile}
}

const acceptedNoEdits = `{"output":{"accepted":true},"edits":[]}`

// Without a pin, the brief's task_scope block is the turn's scope: its files
// are read and editable. An absent --files splits to [""], which pins nothing.
func TestBriefTaskScopeIsTheUnpinnedScope(t *testing.T) {
	for name, files := range map[string][]string{"nil": nil, "absent flag": strings.Split("", ",")} {
		t.Run(name, func(t *testing.T) {
			c, config, l := turnFixture(t, "implement", []any{taskScope([]string{"answer.go", "pkg/answer_test.go"}, "pkg/answer_test.go")}, `{"output":{"accepted":true},"edits":[{"path":"answer.go","content":"package answer\n"}]}`)
			writeFile(t, l.Worktree, "pkg/answer_test.go", "package pkg\n")
			c.Files = files
			if e := Run(context.Background(), c, config); e != nil {
				t.Fatal(e)
			}
			if got := readFile(t, l.Worktree, "answer.go"); got != "package answer\n" {
				t.Fatal("block-scoped edit not applied", got)
			}
			if got := promptFiles(t, l); len(got) != 2 || got["answer.go"] != "original" || got["pkg/answer_test.go"] != "package pkg\n" {
				t.Fatal("block files not sent", got)
			}
		})
	}
	// The frozen test file the block names stays out of an implement turn.
	c, config, l := turnFixture(t, "implement", []any{taskScope([]string{"answer.go", "answer_test.go"}, "answer_test.go")}, `{"output":{"accepted":true},"edits":[{"path":"answer_test.go","content":"package answer\n"}]}`)
	c.Files = nil
	if e := Run(context.Background(), c, config); e == nil || !strings.Contains(e.Error(), "frozen test") {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(l.Worktree, "answer_test.go")); !os.IsNotExist(e) {
		t.Fatal("frozen test file written", e)
	}
}

// A pinned scope (--files or a --task-scopes entry) wins over the block: the
// block's files are neither sent nor editable, never a union.
func TestPinnedScopeWinsOverBriefTaskScope(t *testing.T) {
	for _, pin := range []string{"files", "task-scopes"} {
		t.Run(pin, func(t *testing.T) {
			c, config, l := turnFixture(t, "implement", []any{taskScope([]string{"answer.go", "extra.go", "answer_test.go"}, "answer_test.go")}, `{"output":{"accepted":true},"edits":[{"path":"extra.go","content":"package extra\n"}]}`)
			writeFile(t, l.Worktree, "extra.go", "package extra // block only\n")
			c.Files, c.TestFile = []string{"answer.go", "answer_test.go"}, "answer_test.go"
			if pin == "task-scopes" {
				c.Files, c.TestFile = []string{"unrelated.go"}, ""
				c.TaskScopesJSON = `{"t":{"files":["answer.go","answer_test.go"],"test_file":"answer_test.go"}}`
			}
			if e := Run(context.Background(), c, config); e == nil || !strings.Contains(e.Error(), "unauthorized edit") {
				t.Fatal("block-only file editable under a pin", e)
			}
			if got := readFile(t, l.Worktree, "extra.go"); got != "package extra // block only\n" {
				t.Fatal("block-only file changed", got)
			}
			if got := promptFiles(t, l); len(got) != 2 || got["answer.go"] != "original" {
				t.Fatal("pinned files not the prompt's files", got)
			}
		})
	}
}

// The block's test file must equal the pinned one: a mismatch means the
// daemon and the bridge disagree, and the turn is refused before launch.
func TestBriefTaskScopeTestFileMismatchRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		files    []string
		testFile string
		scopes   string
		block    string
	}{
		"task-scopes entry":   {scopes: `{"t":{"files":["answer.go","a_test.go"],"test_file":"a_test.go"}}`, block: "b_test.go"},
		"files and test file": {files: []string{"answer.go", "a_test.go"}, testFile: "a_test.go", block: "b_test.go"},
		"files without test":  {files: []string{"answer.go", "b_test.go"}, block: "b_test.go"},
	} {
		t.Run(name, func(t *testing.T) {
			c, config, l := turnFixture(t, "test-first", []any{taskScope([]string{"answer.go", tc.block}, tc.block)}, acceptedNoEdits)
			c.Files, c.TestFile, c.TaskScopesJSON = tc.files, tc.testFile, tc.scopes
			if e := Run(context.Background(), c, config); e == nil || !strings.Contains(e.Error(), "differs from the pinned test file") {
				t.Fatal(e)
			}
			if launched(l) {
				t.Fatal("provider launched despite scope mismatch")
			}
		})
	}
	// A pin without a test file serves turns that cannot edit, as it did
	// before blocks existed; only change turns need the two to agree.
	t.Run("files without test on a reading turn", func(t *testing.T) {
		c, config, l := turnFixture(t, "intake", []any{taskScope([]string{"answer.go", "b_test.go"}, "b_test.go")}, acceptedNoEdits)
		c.Files = []string{"answer.go"}
		if e := Run(context.Background(), c, config); e != nil || !launched(l) {
			t.Fatal(e)
		}
	})
	// Agreement, or a block without a test file yet, is accepted.
	for name, block := range map[string]string{"same": "a_test.go", "none yet": ""} {
		t.Run(name, func(t *testing.T) {
			c, config, _ := turnFixture(t, "test-first", []any{taskScope([]string{"answer.go"}, block)}, acceptedNoEdits)
			c.Files, c.TestFile = []string{"answer.go", "a_test.go"}, "a_test.go"
			if block != "" {
				c.Files = append(c.Files, block)
			}
			if e := Run(context.Background(), c, config); e != nil {
				t.Fatal(e)
			}
		})
	}
}

// Every block path must be a safe worktree path; one bad path refuses the
// turn before launch, pinned or not. Inconsistent blocks are refused too.
func TestBriefTaskScopeUnsafeOrInconsistentRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		blocks []any
		pinned []string
		test   string
		want   string
	}{
		"traversal":            {blocks: []any{taskScope([]string{"answer.go", "../escape.go"}, "")}, want: "unsafe"},
		"absolute":             {blocks: []any{taskScope([]string{"/etc/passwd"}, "")}, want: "unsafe"},
		"unclean":              {blocks: []any{taskScope([]string{"pkg/../answer.go"}, "")}, want: "unsafe"},
		"git metadata":         {blocks: []any{taskScope([]string{".git/config"}, "")}, want: "unsafe"},
		"empty":                {blocks: []any{taskScope([]string{""}, "")}, want: "unsafe"},
		"symlink":              {blocks: []any{taskScope([]string{"link/answer.go"}, "")}, want: "symlink"},
		"unsafe test file":     {blocks: []any{taskScope([]string{"answer.go"}, "../a_test.go")}, want: "unsafe"},
		"unsafe under a pin":   {blocks: []any{taskScope([]string{"../escape.go"}, "")}, pinned: []string{"answer.go"}, want: "unsafe"},
		"test file not listed": {blocks: []any{taskScope([]string{"answer.go"}, "answer_test.go")}, want: "not in its files"},
		"two blocks":           {blocks: []any{taskScope([]string{"answer.go"}, ""), taskScope([]string{"answer.go"}, "")}, want: "more than one task_scope"},
		"test file only":       {blocks: []any{taskScope([]string{"answer.go"}, "")}, test: "answer_test.go", want: "requires pinned files"},
	} {
		t.Run(name, func(t *testing.T) {
			c, config, l := turnFixture(t, "implement", tc.blocks, acceptedNoEdits)
			if e := os.Symlink(t.TempDir(), filepath.Join(l.Worktree, "link")); e != nil {
				t.Fatal(e)
			}
			c.Files, c.TestFile = tc.pinned, tc.test
			if e := Run(context.Background(), c, config); e == nil || !strings.Contains(e.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, e)
			}
			if launched(l) {
				t.Fatal("provider launched despite refused scope")
			}
		})
	}
}

// The files sent in one turn are bounded in total, pinned or from the block;
// over the bound the turn is refused with the total named, never truncated.
func TestTaskFilesTotalBound(t *testing.T) {
	var names []string
	for i := 0; i < 5; i++ {
		names = append(names, "big"+string(rune('a'+i))+".go")
	}
	for _, pin := range []bool{true, false} {
		c, config, l := turnFixture(t, "implement", []any{taskScope(names, "")}, acceptedNoEdits)
		for _, name := range names {
			writeFile(t, l.Worktree, name, strings.Repeat("x", 1000*1000))
		}
		if c.Files = nil; pin {
			c.Files = names
		}
		e := Run(context.Background(), c, config)
		if e == nil || !strings.Contains(e.Error(), "task files total 5000000 bytes, over the 4194304-byte") {
			t.Fatal(pin, e)
		}
		if launched(l) {
			t.Fatal("provider launched over the files bound")
		}
	}
	// Exactly at the bound is accepted.
	c, config, l := turnFixture(t, "implement", nil, acceptedNoEdits)
	writeFile(t, l.Worktree, "a.go", strings.Repeat("x", 1<<20))
	writeFile(t, l.Worktree, "b.go", strings.Repeat("x", 1<<20))
	writeFile(t, l.Worktree, "c.go", strings.Repeat("x", 1<<20))
	writeFile(t, l.Worktree, "d.go", strings.Repeat("x", 1<<20))
	c.Files = []string{"a.go", "b.go", "c.go", "d.go"}
	if e := Run(context.Background(), c, config); e != nil {
		t.Fatal(e)
	}
}

// The test-first and implement edit rules, for a test file named by the task
// (pinned --test-file or the block's test_file) and for none yet.
func TestRedAndImplementEditRules(t *testing.T) {
	l := r.BridgeConfig{Launch: r.Launch{Worktree: t.TempDir(), Profile: workflow.Profile{Allowed: []string{"handoff", "edit"}}}}
	files := []string{"answer.go", "answer_test.go", "other_test.go", "pkg/new_test.go", "_test.go", "pkg/.hidden_test.go"}
	for _, tc := range []struct {
		stage, testFile string
		edits           []string
		ok              bool
	}{
		{"test-first", "", []string{"answer_test.go"}, true},
		{"test-first", "", []string{"pkg/new_test.go"}, true},
		{"test-first", "", nil, true},
		{"test-first", "", []string{"answer.go"}, false},
		{"test-first", "", []string{"answer_test.go", "other_test.go"}, false},
		{"test-first", "", []string{"outside_test.go"}, false},
		{"test-first", "", []string{"_test.go"}, false},
		{"test-first", "", []string{"pkg/.hidden_test.go"}, false},
		{"test-first", "answer_test.go", []string{"answer_test.go"}, true},
		{"test-first", "answer_test.go", []string{"other_test.go"}, false},
		{"test-first", "answer_test.go", []string{"answer.go"}, false},
		{"implement", "answer_test.go", []string{"answer.go", "other_test.go"}, true},
		{"implement", "answer_test.go", []string{"answer_test.go"}, false},
		{"implement", "answer_test.go", []string{"outside.go"}, false},
		{"implement", "", []string{"answer.go", "answer_test.go"}, true},
		{"review", "", []string{"answer.go"}, false},
		{"spec", "", []string{"answer_test.go"}, false},
	} {
		var edits []string
		for _, path := range tc.edits {
			edits = append(edits, `{"path":"`+path+`","content":"x","replacements":[]}`)
		}
		response := decodeResponse(t, `{"edits":[`+strings.Join(edits, ",")+`]}`)
		err := validateEdits(Config{Files: files, TestFile: tc.testFile}, l, tc.stage, response)
		if (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc, err)
		}
	}
	// Each edit uses exactly one form.
	for raw, ok := range map[string]bool{
		`{"path":"answer.go","content":"x","replacements":[]}`:                       true,
		`{"path":"answer.go","content":null,"replacements":[{"old":"a","new":"b"}]}`: true,
		`{"path":"answer.go","content":"x","replacements":[{"old":"a","new":"b"}]}`:  false,
		`{"path":"answer.go","content":null,"replacements":[]}`:                      false,
		`{"path":"answer.go"}`: false,
	} {
		if err := validateEdits(Config{Files: files}, l, "implement", decodeResponse(t, `{"edits":[`+raw+`]}`)); (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

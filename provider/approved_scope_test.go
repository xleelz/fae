package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	r "github.com/xleelz/fae/runtime"
)

// approvedTurn is turnFixture for an implement turn whose stage block carries
// the daemon-approved scope-widen files.
func approvedTurn(t *testing.T, approved []string, blocks []any, answer string) (Config, string, r.BridgeConfig) {
	t.Helper()
	c, config, l := turnFixture(t, "implement", blocks, answer)
	var brief []map[string]any
	if e := json.Unmarshal(l.Brief, &brief); e != nil {
		t.Fatal(e)
	}
	brief[0]["approved_scope"] = approved
	l.Brief, _ = json.Marshal(brief)
	writeLaunch(t, config, l)
	return c, config, l
}

// listedUnavailable is one entry of a prompt's unavailable_scope block.
type listedUnavailable struct{ Path, Reason string }

// promptUnavailable returns the unavailable_scope block of the turn's prompt.
func promptUnavailable(t *testing.T, l r.BridgeConfig) []listedUnavailable {
	t.Helper()
	block, ok := promptBlocks(t, []byte(providerFile(t, l.LogDirectory, "prompt.json")))["unavailable_scope"]
	if !ok {
		return nil
	}
	var b struct{ Files []listedUnavailable }
	if e := json.Unmarshal(block, &b); e != nil {
		t.Fatal(e)
	}
	return b.Files
}

// FAE-WF-078: an approved scope-widen file the bridge can't load (a symlink on
// its path, a non-regular file, one over 1 MiB, or one that takes the turn's
// files over 4 MiB) no longer refuses the turn. Nothing removes approved scope,
// so that refusal recurred on every later turn of the task, intake included.
// The turn runs; the file is listed with the reason, never read (a symlink out
// of the worktree is not followed) and not sent. Pinned and derived scopes
// alike.
func TestApprovedWidenFileTheBridgeCannotLoadIsListedNotRefused(t *testing.T) {
	const secret = "outside the worktree"
	cases := map[string]struct {
		name, reason string
		declared     []string
		plant        func(t *testing.T, root string)
	}{
		"symlinked directory": {"link/extra.go", "symlink", nil, func(t *testing.T, root string) {
			outside := t.TempDir()
			writeFile(t, outside, "extra.go", secret)
			if e := os.Symlink(outside, filepath.Join(root, "link")); e != nil {
				t.Fatal(e)
			}
		}},
		"symlinked file": {"alias.go", "symlink", nil, func(t *testing.T, root string) {
			outside := t.TempDir()
			writeFile(t, outside, "extra.go", secret)
			if e := os.Symlink(filepath.Join(outside, "extra.go"), filepath.Join(root, "alias.go")); e != nil {
				t.Fatal(e)
			}
		}},
		"directory": {"dir.go", "bounded regular file", nil, func(t *testing.T, root string) {
			if e := os.Mkdir(filepath.Join(root, "dir.go"), 0700); e != nil {
				t.Fatal(e)
			}
		}},
		"over 1 MiB": {"big.go", "bounded regular file", nil, func(t *testing.T, root string) {
			writeFile(t, root, "big.go", strings.Repeat("x", 1<<20+1))
		}},
		"over the turn's total": {"d.go", "over the 4194304-byte provider input bound", []string{"a.go", "b.go", "c.go"}, func(t *testing.T, root string) {
			for _, name := range []string{"a.go", "b.go", "c.go", "d.go"} {
				writeFile(t, root, name, strings.Repeat("x", 1<<20))
			}
		}},
	}
	for name, tc := range cases {
		for _, mode := range []string{"pinned", "derived"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				declared := append([]string{"answer.go"}, tc.declared...)
				var blocks []any
				if mode == "derived" {
					blocks = []any{taskScope(append(append([]string(nil), declared...), tc.name), "")}
				}
				c, config, l := approvedTurn(t, []string{tc.name}, blocks, acceptedNoEdits)
				tc.plant(t, l.Worktree)
				c.Files = declared
				if mode == "derived" {
					c.Files = nil
				}
				if e := Run(context.Background(), c, config); e != nil {
					t.Fatal("an approved file the bridge cannot load refused the turn:", e)
				}
				if !launched(l) {
					t.Fatal("provider not launched")
				}
				files := promptFiles(t, l)
				if _, sent := files[tc.name]; sent || files["answer.go"] != "original" {
					t.Fatalf("files block %v", keys(files))
				}
				unavailable := promptUnavailable(t, l)
				if len(unavailable) != 1 || unavailable[0].Path != tc.name || !strings.Contains(unavailable[0].Reason, tc.reason) {
					t.Fatalf("unavailable block %+v, want %s (%s)", unavailable, tc.name, tc.reason)
				}
				if strings.Contains(providerFile(t, l.LogDirectory, "prompt.json"), secret) {
					t.Fatal("a symlink out of the worktree was followed")
				}
			})
		}
	}
}

// keys lists a map's keys for a failure message.
func keys(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

// An unavailable approved file is not editable in the turn: an edit to it is
// refused output and nothing is written. A declared file the bridge can't
// load still refuses the turn.
func TestUnavailableApprovedFileIsNotEditable(t *testing.T) {
	c, config, l := approvedTurn(t, []string{"big.go"}, nil, `{"output":{"accepted":true},"edits":[{"path":"big.go","content":"x"}]}`)
	big := strings.Repeat("x", 1<<20+1)
	writeFile(t, l.Worktree, "big.go", big)
	e := Run(context.Background(), c, config)
	var refusal *Refusal
	if !errors.As(e, &refusal) || refusal.Class != r.RefusedOutput || !strings.Contains(e.Error(), "unauthorized edit") {
		t.Fatalf("an edit to an unavailable file was not refused as output: %v", e)
	}
	if readFile(t, l.Worktree, "big.go") != big {
		t.Fatal("unavailable file written")
	}

	c, config, l = approvedTurn(t, nil, nil, acceptedNoEdits)
	writeFile(t, l.Worktree, "declared.go", strings.Repeat("x", 1<<20+1))
	c.Files = []string{"answer.go", "declared.go"}
	if e := Run(context.Background(), c, config); e == nil || !strings.Contains(e.Error(), "bounded regular file") || launched(l) {
		t.Fatalf("an unloadable declared file did not refuse the turn: %v", e)
	}
}

// The stage block's approved_scope follows the one declared-path rule like
// every other scope path the bridge reads (workflow.DeclaredPathSafe): a path
// inside .git at any depth refuses the turn before the provider runs. The
// daemon never sends one; this is defence in depth.
func TestBridgeRefusesUnsafeApprovedScope(t *testing.T) {
	for _, name := range []string{"sub/.git/config", ".GIT/HEAD", "../escape.go"} {
		c, config, l := approvedTurn(t, []string{name}, nil, acceptedNoEdits)
		e := Run(context.Background(), c, config)
		var refusal *Refusal
		if !errors.As(e, &refusal) || refusal.Class != r.RefusedBeforeProvider || !strings.Contains(e.Error(), "unsafe") {
			t.Fatalf("%s: want a local refusal naming the unsafe path, got %v", name, e)
		}
		if launched(l) {
			t.Fatal("provider launched despite an unsafe approved path")
		}
	}
}

// FAE-WF-078: an approved scope-widen file that is the turn's RED test file
// and that the bridge can't load (here a symlinked directory on its path, or
// over 1 MiB) no longer refuses every turn of the task. Before, it was the
// one approved file still refused, so every later turn failed and only
// Retire ended the task. It is listed as unavailable like any approved file:
// not read (a symlink is not followed), not sent, not editable. The RED
// identity is unchanged (the task_scope block still names the test file), the
// engine runs the RED test from the worktree, and a test-first turn can only
// report the RED as it stands: an edit to it is refused output.
func TestUnloadableApprovedRedFileIsListedNotRefused(t *testing.T) {
	const red = "pkg/answer_test.go"
	scope := []any{taskScope([]string{"answer.go", red}, red)}
	for name, plant := range map[string]func(t *testing.T, root string) string{
		"symlinked directory": func(t *testing.T, root string) string {
			outside := t.TempDir()
			writeFile(t, outside, "answer_test.go", "outside the worktree")
			if e := os.Symlink(outside, filepath.Join(root, "pkg")); e != nil {
				t.Fatal(e)
			}
			return "symlink"
		},
		"over 1 MiB": func(t *testing.T, root string) string {
			writeFile(t, root, red, strings.Repeat("x", 1<<20+1))
			return "bounded regular file"
		},
	} {
		for _, stage := range []string{"implement", "spec", "test-first"} {
			t.Run(name+"/"+stage, func(t *testing.T) {
				c, config, l := turnFixture(t, stage, scope, acceptedNoEdits)
				var brief []map[string]any
				if e := json.Unmarshal(l.Brief, &brief); e != nil {
					t.Fatal(e)
				}
				brief[0]["approved_scope"] = []string{red}
				l.Brief, _ = json.Marshal(brief)
				writeLaunch(t, config, l)
				reason := plant(t, l.Worktree)
				c.Files = nil
				if e := Run(context.Background(), c, config); e != nil {
					t.Fatal("an unloadable approved RED test file refused the turn:", e)
				}
				if !launched(l) {
					t.Fatal("provider not launched")
				}
				if files := promptFiles(t, l); files["answer.go"] != "original" || files[red] != "" {
					t.Fatalf("files block %v", keys(files))
				}
				unavailable := promptUnavailable(t, l)
				if len(unavailable) != 1 || unavailable[0].Path != red || !strings.Contains(unavailable[0].Reason, reason) {
					t.Fatalf("unavailable block %+v, want %s (%s)", unavailable, red, reason)
				}
				if strings.Contains(providerFile(t, l.LogDirectory, "prompt.json"), "outside the worktree") {
					t.Fatal("a symlink out of the worktree was followed")
				}
			})
		}
	}
	// The test-first turn cannot rewrite the RED file it was not sent.
	c, config, l := turnFixture(t, "test-first", scope, `{"output":{"accepted":true},"edits":[{"path":"`+red+`","content":"func TestX(t *testing.T) {}","replacements":[]}]}`)
	var brief []map[string]any
	if e := json.Unmarshal(l.Brief, &brief); e != nil {
		t.Fatal(e)
	}
	brief[0]["approved_scope"] = []string{red}
	l.Brief, _ = json.Marshal(brief)
	writeLaunch(t, config, l)
	big := strings.Repeat("x", 1<<20+1)
	writeFile(t, l.Worktree, red, big)
	c.Files = nil
	e := Run(context.Background(), c, config)
	var refusal *Refusal
	if !errors.As(e, &refusal) || refusal.Class != r.RefusedOutput || !strings.Contains(e.Error(), "unauthorized edit") {
		t.Fatalf("an edit to the unavailable RED file was not refused as output: %v", e)
	}
	if readFile(t, l.Worktree, red) != big {
		t.Fatal("the unavailable RED file was written")
	}
}

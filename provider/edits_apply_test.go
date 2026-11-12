package provider

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	r "github.com/xleelz/fae/runtime"
)

// editTurnFiles is what a test reads back of the worktree a turn's edits
// targeted: each path's content and mode, or "absent".
func editTurnFiles(t *testing.T, root string, names ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range names {
		st, e := os.Lstat(filepath.Join(root, name))
		if os.IsNotExist(e) {
			out[name] = "absent"
			continue
		}
		if e != nil {
			t.Fatal(e)
		}
		if st.IsDir() {
			out[name] = "directory " + st.Mode().Perm().String()
			continue
		}
		out[name] = st.Mode().Perm().String() + " " + readFile(t, root, name)
	}
	return out
}

// stagedEditFiles lists the bridge's staged edit files left in the worktree.
func stagedEditFiles(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if e == nil && strings.HasPrefix(d.Name(), ".fae-edit-") {
			found = append(found, path)
		}
		return nil
	})
	return found
}

// FAE-ROB-012, FAE-ROB-016, FAE-VCS-003: a completed turn's edits are written
// all or nothing. Here the third of four edits cannot be written (its
// directory is read-only). Before, the first two were already written when
// the third failed, the turn ended untyped with no handoff, the task parked
// UncertainDispatch, and redispatch and restart were refused because the
// tree no longer matched the checkpoint. Now nothing in the worktree
// changes: no edited file, no new directory, no staged file and no handoff,
// and the turn is a typed edits-not-applied refusal the executor retries.
func TestBridgeEditBatchWhoseThirdWriteFailsLeavesTheTreeUnchanged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root")
	}
	answer := `{"output":{"accepted":true},"edits":[` +
		`{"path":"answer.go","content":"rewritten","replacements":[]},` +
		`{"path":"b.go","content":null,"replacements":[{"old":"original","new":"replaced"}]},` +
		`{"path":"ro/c.go","content":"cannot be written","replacements":[]},` +
		`{"path":"sub/new.go","content":"new file","replacements":[]}]}`
	c, config, l := turnFixture(t, "implement", nil, answer)
	writeFile(t, l.Worktree, "b.go", "b original")
	if e := os.Chmod(filepath.Join(l.Worktree, "b.go"), 0755); e != nil {
		t.Fatal(e)
	}
	ro := filepath.Join(l.Worktree, "ro")
	if e := os.Mkdir(ro, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(ro, 0500); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0700) })
	c.Files = []string{"answer.go", "b.go", "ro/c.go", "sub/new.go"}
	names := []string{"answer.go", "b.go", "ro/c.go", "sub", "sub/new.go"}
	before := editTurnFiles(t, l.Worktree, names...)

	e := Run(context.Background(), c, config)
	var refusal *Refusal
	if !errors.As(e, &refusal) || refusal.Class != "edits-not-applied" || !strings.Contains(e.Error(), `edit "ro/c.go"`) || !strings.Contains(e.Error(), "the worktree is as the turn found it") {
		t.Fatalf("a failed third write is not a typed edits-not-applied refusal naming it: %v", e)
	}
	if ExitCode(e) != r.BridgeRefusalExit {
		t.Fatal("the refusal was not recorded", ExitCode(e))
	}
	if after := editTurnFiles(t, l.Worktree, names...); strings.Join(mapLines(after), "\n") != strings.Join(mapLines(before), "\n") {
		t.Fatalf("the worktree changed:\nbefore %v\nafter  %v", before, after)
	}
	if staged := stagedEditFiles(t, l.Worktree); len(staged) != 0 {
		t.Fatal("staged edit files left in the worktree", staged)
	}
	if _, e := os.Lstat(l.Handoff); !os.IsNotExist(e) {
		t.Fatal("a handoff was left for edits that were not applied", e)
	}
	if !strings.Contains(readFile(t, l.LogDirectory, r.BridgeRefusalFile), "edits-not-applied") {
		t.Fatal("the refusal record does not carry the class")
	}
}

// mapLines is a map's entries as sorted "key: value" lines.
func mapLines(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+": "+v)
	}
	sort.Strings(out)
	return out
}

// An accepted batch is written as before: whole-file and replacement edits,
// a new file in new directories, each edited file keeping its mode (a new one
// is private), the handoff written last, and no staged file left behind.
func TestBridgeEditBatchAppliesEveryEditAndKeepsModes(t *testing.T) {
	answer := `{"output":{"accepted":true},"edits":[` +
		`{"path":"answer.go","content":"rewritten","replacements":[]},` +
		`{"path":"run.sh","content":null,"replacements":[{"old":"original","new":"replaced"}]},` +
		`{"path":"a/b/new.go","content":"new file","replacements":[]}]}`
	c, config, l := turnFixture(t, "implement", nil, answer)
	writeFile(t, l.Worktree, "run.sh", "run original")
	if e := os.Chmod(filepath.Join(l.Worktree, "run.sh"), 0755); e != nil {
		t.Fatal(e)
	}
	c.Files = []string{"answer.go", "run.sh", "a/b/new.go"}
	if e := Run(context.Background(), c, config); e != nil {
		t.Fatal(e)
	}
	got := editTurnFiles(t, l.Worktree, "answer.go", "run.sh", "a/b/new.go")
	want := map[string]string{"answer.go": "-rw------- rewritten", "run.sh": "-rwxr-xr-x run replaced", "a/b/new.go": "-rw------- new file"}
	if strings.Join(mapLines(got), "\n") != strings.Join(mapLines(want), "\n") {
		t.Fatalf("applied edits %v, want %v", got, want)
	}
	if staged := stagedEditFiles(t, l.Worktree); len(staged) != 0 {
		t.Fatal("staged edit files left in the worktree", staged)
	}
	if !strings.Contains(readFile(t, l.Worktree, "handoff.json"), `"accepted":true`) {
		t.Fatal("no handoff")
	}
}

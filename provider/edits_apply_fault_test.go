package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	r "github.com/xleelz/fae/runtime"
)

// failEditStep makes applyEdits fail one step for the rest of the test.
func failEditStep(t *testing.T, step string, index int) {
	t.Helper()
	editApplyFault = func(s string, i int) error {
		if s == step && i == index {
			return errors.New("injected " + step + " failure")
		}
		return nil
	}
	t.Cleanup(func() { editApplyFault = nil })
}

// FAE-ROB-012, FAE-ROB-016, FAE-VCS-003: a failure after some edits were
// already renamed into place, or of the handoff write after all of them,
// restores every renamed file's content and mode from before the turn,
// removes a file the turn created and the directories made for it, and
// leaves no staged file and no handoff. The turn is a typed edits-not-applied
// refusal naming the failure.
func TestBridgeEditRollbackRestoresRenamedFiles(t *testing.T) {
	answer := `{"output":{"accepted":true},"edits":[` +
		`{"path":"answer.go","content":"rewritten","replacements":[]},` +
		`{"path":"run.sh","content":null,"replacements":[{"old":"original","new":"replaced"}]},` +
		`{"path":"a/b/new.go","content":"new file","replacements":[]},` +
		`{"path":"last.go","content":"last","replacements":[]}]}`
	names := []string{"answer.go", "run.sh", "a", "a/b/new.go", "last.go"}
	for _, tc := range []struct {
		step  string
		index int
	}{{"stage", 2}, {"rename", 2}, {"rename", 3}, {"commit", -1}} {
		name := fmt.Sprintf("%s edit %d", tc.step, tc.index+1)
		if tc.index < 0 {
			name = "handoff"
		}
		t.Run(name, func(t *testing.T) {
			c, config, l := turnFixture(t, "implement", nil, answer)
			writeFile(t, l.Worktree, "run.sh", "run original")
			if e := os.Chmod(filepath.Join(l.Worktree, "run.sh"), 0755); e != nil {
				t.Fatal(e)
			}
			c.Files = []string{"answer.go", "run.sh", "a/b/new.go", "last.go"}
			before := editTurnFiles(t, l.Worktree, names...)
			failEditStep(t, tc.step, tc.index)
			e := Run(context.Background(), c, config)
			var refusal *Refusal
			if !errors.As(e, &refusal) || refusal.Class != r.EditsNotApplied || !strings.Contains(e.Error(), "injected "+tc.step+" failure") {
				t.Fatalf("not a typed edits-not-applied refusal naming the failure: %v", e)
			}
			if after := editTurnFiles(t, l.Worktree, names...); strings.Join(mapLines(after), "\n") != strings.Join(mapLines(before), "\n") {
				t.Fatalf("the worktree was not restored:\nbefore %v\nafter  %v", before, after)
			}
			if staged := stagedEditFiles(t, l.Worktree); len(staged) != 0 {
				t.Fatal("staged edit files left in the worktree", staged)
			}
			if _, e := os.Lstat(l.Handoff); !os.IsNotExist(e) {
				t.Fatal("a handoff was left for edits that were not applied", e)
			}
		})
	}
}

// A batch the worktree cannot take is refused before anything is written:
// an edit whose target is a directory, one whose target the bridge may not
// write (a rename could replace it, an in-place write could not), one under a
// path that is a file, and two edits that need one path to be both a file
// and a directory.
func TestBridgeEditBatchCheckedBeforeAnyWrite(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "answer.go", "original")
	writeFile(t, root, "file.go", "a file")
	writeFile(t, root, "locked.go", "locked")
	if e := os.Chmod(filepath.Join(root, "locked.go"), 0400); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(filepath.Join(root, "dir.go"), 0700); e != nil {
		t.Fatal(e)
	}
	content := func(s string) *string { return &s }
	for name, edits := range map[string][]Edit{
		"target is a directory":      {{Path: "answer.go", Content: content("x")}, {Path: "dir.go", Content: content("x")}},
		"target is not writable":     {{Path: "answer.go", Content: content("x")}, {Path: "locked.go", Content: content("x")}},
		"parent is a file":           {{Path: "answer.go", Content: content("x")}, {Path: "file.go/x.go", Content: content("x")}},
		"file and directory at once": {{Path: "answer.go", Content: content("x")}, {Path: "pkg", Content: content("x")}, {Path: "pkg/x.go", Content: content("x")}},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "target is not writable" && os.Geteuid() == 0 {
				t.Skip("root writes read-only files")
			}
			contents := make([][]byte, len(edits))
			for i := range edits {
				contents[i] = []byte(*edits[i].Content)
			}
			committed := false
			e := applyEdits(root, edits, contents, map[string][]byte{"answer.go": []byte("original")}, func() error { committed = true; return nil })
			var refusal *Refusal
			if !errors.As(e, &refusal) || refusal.Class != r.EditsNotApplied || committed {
				t.Fatalf("not refused before any write: %v (committed %t)", e, committed)
			}
			if readFile(t, root, "answer.go") != "original" || readFile(t, root, "locked.go") != "locked" || len(stagedEditFiles(t, root)) != 0 {
				t.Fatal("an edit was written")
			}
			if _, e := os.Lstat(filepath.Join(root, "pkg")); !os.IsNotExist(e) {
				t.Fatal("a directory was created", e)
			}
		})
	}
}

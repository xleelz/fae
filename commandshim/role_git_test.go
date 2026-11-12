package commandshim

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoleGitQueriesStayInsideOwnWorktree(t *testing.T) {
	root := t.TempDir()
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(binary, "-C", root, "init").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	s := RoleShims{Worktree: root, Local: true, Tools: map[string]string{"git": binary}}
	run := func(args ...string) (int, error) {
		var out bytes.Buffer
		return runRoleGit(context.Background(), s, HeavyOp{Argv: append([]string{"git"}, args...), Dir: root, Env: []string{"PATH=/usr/bin:/bin"}, Stdout: &out, Stderr: &out})
	}
	for _, args := range [][]string{{"rev-parse", "--show-toplevel"}, {"status"}, {"diff"}, {"ls-files"}} {
		if code, err := run(args...); err != nil || code != 0 {
			t.Errorf("query %v: %d %v", args, code, err)
		}
	}
	for _, args := range [][]string{{"commit", "-m", "bad"}, {"checkout", "-b", "bad"}, {"worktree", "add", filepath.Join(t.TempDir(), "bad")}, {"-C", t.TempDir(), "log"}, {"status", "--output=" + filepath.Join(t.TempDir(), "bad")}} {
		if code, _ := run(args...); code != Refused {
			t.Errorf("mutation/escape %v accepted code%d", args, code)
		}
	}
	if code, _ := run("push"); code != LocalModeRefused {
		t.Fatal("Local push code", code)
	}
	// Exact policy refusals remain guard-censused after read queries were enabled.
	if code, err := run("commit", "-m", "forbidden"); code != Refused || err == nil || !strings.Contains(err.Error(), "role sessions allow read-only git queries; the engine's VCS broker commits") {
		t.Fatal(code, err)
	}
	s.Local = false
	if code, err := run("push"); code != Refused || err == nil || !strings.Contains(err.Error(), "role git remote commands require engine authority") {
		t.Fatal(code, err)
	}
	outside := t.TempDir()
	if code, err := runRoleGit(context.Background(), s, HeavyOp{Argv: []string{"git", "status"}, Dir: outside, Env: []string{"PATH=/usr/bin:/bin"}}); code != Refused || err == nil || !strings.Contains(err.Error(), "git query outside the session worktree") {
		t.Fatal(code, err)
	}
	if _, err = os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
}

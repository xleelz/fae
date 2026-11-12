package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/worktree"
)

// A derived RedReport is checked when its test-first turn completes. A
// malformed one is a ValidationFailed refusal, and the turn's RED file is put
// back to the dispatch tree, whether the turn created it or changed it, so
// the retry starts from the recorded tree. A turn whose change the RED rule
// refuses is left as it is. A well-formed report, a pinned scope and another
// stage's output are not refused.
func TestLiveRedReportCheckedAtTurnCompletion(t *testing.T) {
	ctx := context.Background()
	root := reviewPhysical(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	liveGit(t, repo, "init")
	original := "package sample\n\nfunc answer() int { return 0 }\n"
	if err := os.WriteFile(filepath.Join(repo, "answer.go"), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, repo, "add", ".")
	liveGit(t, repo, "commit", "-m", "base")
	manager := &worktree.Manager{Repository: repo, Root: filepath.Join(root, "trees")}
	w, err := manager.Ensure(ctx, "run", "task", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{jobs: map[string]controlplane.JobSnapshot{}}
	x := &liveExecutor{daemon: daemon, manager: manager, broker: &effects.Broker{Manager: manager}}
	setJob := func(e envelope) {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		daemon.jobs["task"] = controlplane.JobSnapshot{ID: "task", Payload: raw}
	}
	tree := func() string {
		t.Helper()
		got, err := x.broker.Tree(ctx, w)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(w.Path, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	report := func(id, expected string, files ...string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"test_id": id, "test_shape": "go-unit", "declared_files": files, "expected_failure": expected, "source": liveTestSource})
		return raw
	}
	doc := defaults.Workflow()
	declared := []string{"answer.go", "answer_test.go", "other_test.go"}
	setJob(scopeEnvelope(declared))
	d := engine.Dispatch{TaskID: "task", Tree: tree(), Config: doc.Stages["test-first"]}
	b := execution.Binding{Worktree: w.Path}
	write("answer_test.go", liveTestSource)
	if err = x.checkRedReport(ctx, d, b, report("TestBehavior", "got 0 want 42", "answer.go", "answer_test.go")); err != nil {
		t.Fatal("well-formed RedReport refused", err)
	}
	if tree() == d.Tree {
		t.Fatal("accepted RED turn restored")
	}
	for name, output := range map[string]json.RawMessage{
		"no test file":       report("TestBehavior", "got 0 want 42", "answer.go"),
		"two test files":     report("TestBehavior", "got 0 want 42", "answer_test.go", "other_test.go"),
		"non-Go test id":     report("test_behavior", "got 0 want 42", "answer_test.go"),
		"no expected reason": report("TestBehavior", " ", "answer_test.go"),
		"test id as reason":  report("TestBehavior", "TestBehavior", "answer_test.go"),
		"framework reason":   report("TestBehavior", "--- FAIL: TestBehavior", "answer_test.go"),
		"not an object":      json.RawMessage(`[]`),
	} {
		write("answer_test.go", liveTestSource)
		err := x.checkRedReport(ctx, d, b, output)
		if !isScopeRefusal(err) || !strings.Contains(err.Error(), "RedReport refused at turn completion") {
			t.Errorf("%s: %v", name, err)
		}
		if _, statErr := os.Lstat(filepath.Join(w.Path, "answer_test.go")); tree() != d.Tree || !os.IsNotExist(statErr) {
			t.Errorf("%s: refused RED turn not restored: %v", name, statErr)
		}
	}
	// A RED rework that changed the existing RED file gets its bytes back.
	write("answer_test.go", liveTestSource)
	liveGit(t, w.Path, "add", ".")
	liveGit(t, w.Path, "commit", "-m", "red")
	setJob(scopeEnvelope(declared, redEvidence("TestBehavior", "answer_test.go")))
	d.Tree = tree()
	write("answer_test.go", strings.Replace(liveTestSource, "!= 42", "!= 43", 1))
	if err = x.checkRedReport(ctx, d, b, report("TestBehavior", "FAIL", "answer_test.go")); !isScopeRefusal(err) {
		t.Fatal("vacuous rework RedReport accepted", err)
	}
	if got, _ := os.ReadFile(filepath.Join(w.Path, "answer_test.go")); string(got) != liveTestSource || tree() != d.Tree {
		t.Fatalf("rework RED file not restored: %q", got)
	}
	// A change the RED rule refuses is never touched here.
	write("answer.go", "package sample\n\nfunc answer() int { return 42 }\n")
	if err = x.checkRedReport(ctx, d, b, report("TestBehavior", "got 0 want 42", "answer.go")); err == nil || !strings.Contains(err.Error(), "not restored") {
		t.Fatal("refused change restored", err)
	}
	if got, _ := os.ReadFile(filepath.Join(w.Path, "answer.go")); !strings.Contains(string(got), "return 42") {
		t.Fatal("production change discarded")
	}
	write("answer.go", original)
	// Another stage's output, and a pinned scope, are not read.
	implement := d
	implement.Config = doc.Stages["implement"]
	if err = x.checkRedReport(ctx, implement, b, json.RawMessage(`{}`)); err != nil {
		t.Fatal("implementation report read as a RedReport", err)
	}
	x.config.Files, x.config.TestFile, x.config.TestID, x.config.ExpectedFailure = []string{"answer.go", "answer_test.go"}, "answer_test.go", "TestBehavior", "got 0 want 42"
	pinnedScope, scopeErr := x.dispatchLiveScope(d)
	if scopeErr != nil || pinnedScope.Derived || len(pinnedScope.Files) != 2 || pinnedScope.TestFile != "answer_test.go" {
		t.Fatal("write authority layering changed pinned RED input metadata", pinnedScope, scopeErr)
	}
	if err = x.checkRedReport(ctx, d, b, report("TestBehavior", "FAIL", "answer.go")); err != nil {
		t.Fatal("pinned RedReport refused", err)
	}
}

// DeclaredPathSafe is the one declared-path rule: relative, clean, bounded,
// and never a .git component at any depth or letter case.
func TestDeclaredPathSafe(t *testing.T) {
	for _, name := range []string{"a.go", "pkg/a_test.go", ".gitignore", ".github/workflows/test.yml", "sub/.gitkeep", "x.git/y.go", "a b.go", strings.Repeat("a", 4096)} {
		if !DeclaredPathSafe(name) {
			t.Errorf("%q refused", name)
		}
	}
	for _, name := range []string{"", ".", "..", "/abs.go", "../up.go", "a/../b.go", "./a.go", "a//b.go", "a/", "a\x00.go", "a\n.go", ".git", ".git/config", "sub/.git/config", "sub/.git", ".GIT/config", "a/.Git/HEAD", strings.Repeat("a", 4097)} {
		if DeclaredPathSafe(name) {
			t.Errorf("%q accepted", name)
		}
	}
}

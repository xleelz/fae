package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/workflow"
)

func TestLiveTaskOverridesArePinnedAndIsolated(t *testing.T) {
	a := LiveTaskConfig{Files: []string{"a.go", "a_test.go"}, TestFile: "a_test.go", TestID: "TestA", ExpectedFailure: "a is zero"}
	b := LiveTaskConfig{Files: []string{"b.go", "b_test.go"}, TestFile: "b_test.go", TestID: "TestB", ExpectedFailure: "b is zero"}
	tasks := map[string]LiveTaskConfig{"a": a, "b": b}
	if err := validateLiveTasks(tasks); err != nil {
		t.Fatal(err)
	}
	x := &liveExecutor{config: LiveConfig{Tasks: tasks, Files: []string{"default.go", "default_test.go"}, TestFile: "default_test.go"}}
	scope := func(id string) liveTaskScope {
		s, err := x.resolveLiveTaskScope(id, nil, workflow.Stage{}, nil)
		if err != nil {
			t.Fatal(id, err)
		}
		return s
	}
	first, second, unknown := scope("a"), scope("b"), scope("unknown")
	if first.TestID != "TestA" || second.TestID != "TestB" || x.config.TestFile != "default_test.go" || unknown.TestFile != "default_test.go" || unknown.Derived || first.Derived {
		t.Fatal("scope leaked")
	}
	first.Files[0] = "changed.go"
	if tasks["a"].Files[0] != "a.go" {
		t.Fatal("mutated pinned map")
	}
	for _, name := range []string{"../outside", "/absolute", ".git/config", ".", ""} {
		bad := a
		bad.Files = []string{name, "a_test.go"}
		if validateLiveTasks(map[string]LiveTaskConfig{"a": bad}) == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	for _, bad := range []LiveTaskConfig{{Files: []string{"a.go"}, TestFile: "a_test.go", TestID: "TestA", ExpectedFailure: "a"}, {Files: a.Files, TestFile: a.TestFile, TestID: a.TestID}} {
		if validateLiveTasks(map[string]LiveTaskConfig{"a": bad}) == nil {
			t.Fatal("incomplete RED binding accepted")
		}
	}
}

// The broker narrows declared-shape runs to exactly the pinned test, so a
// pinned RED identity must be one literal identifier, never a pattern; Go
// shapes further require a Go test name when they run (FAE-WF-036).
func TestLiveTaskTestIDIsGoTestName(t *testing.T) {
	ok := LiveTaskConfig{Files: []string{"a.py", "test_a.py"}, TestFile: "test_a.py", TestID: "test_a.py::test_b", ExpectedFailure: "a"}
	if err := validateLiveTasks(map[string]LiveTaskConfig{"a": ok}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"TestA|TestB", "test_*", "-v", "Test A", "TestÄ", "^TestA$"} {
		bad := LiveTaskConfig{Files: []string{"a.go", "a_test.go"}, TestFile: "a_test.go", TestID: id, ExpectedFailure: "a"}
		if validateLiveTasks(map[string]LiveTaskConfig{"a": bad}) == nil {
			t.Errorf("accepted test id %q", id)
		}
	}
}

// Appendix C step 12: only formatting debt in items the task changed counts;
// debt already present at the task base never blocks the task.
func TestFormatDebtCountsChangedItemsOnly(t *testing.T) {
	ctx := context.Background()
	repo := reviewPhysical(t)
	liveGit(t, repo, "init")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("old.go", "package sample\nfunc  old() {}\n")
	write("kept.go", "package sample\n")
	liveGit(t, repo, "add", ".")
	liveGit(t, repo, "commit", "-m", "base with existing debt")
	base := liveGit(t, repo, "rev-parse", "HEAD")
	write("new.go", "package sample\nfunc  added() {}\n")
	write("kept.go", "package sample\n\nfunc kept() {}\n")
	liveGit(t, repo, "add", ".")
	tree := liveGit(t, repo, "write-tree")
	listed := filepath.Join(repo, "old.go") + "\n" + filepath.Join(repo, "new.go") + "\n"
	if debt, err := formatDebt(ctx, repo, base, tree, listed, 0); err != nil || debt != 1 {
		t.Fatal("changed-item debt", debt, err)
	}
	if debt, err := formatDebt(ctx, repo, base, tree, filepath.Join(repo, "old.go")+"\n", 0); err != nil || debt != 0 {
		t.Fatal("pre-existing debt blocked the task", debt, err)
	}
	if debt, err := formatDebt(ctx, repo, base, tree, "", 2); err != nil || debt != 1 {
		t.Fatal("formatter failure not counted", debt, err)
	}
	if _, err := formatDebt(ctx, repo, "", tree, listed, 0); err == nil {
		t.Fatal("missing base accepted")
	}
}

// FAE-WF-030/-076: a non-Go RED is frozen by the token stream of the
// declaration naming the test identifier: a formatter pass or an added test
// keeps it, a changed assertion does not.
func TestRedIdentityNonGoUsesDeclarationTokens(t *testing.T) {
	source := []byte("def test_b():\n    assert answer() == 42\n")
	id, err := redIdentity("tests/test_a.py", source, "test_b")
	if err != nil || !strings.HasPrefix(id, "tokens:") {
		t.Fatal(id, err)
	}
	reformatted := []byte("def test_b():\n  assert answer()  ==  42\n\ndef test_c():\n    pass\n")
	if other, _ := redIdentity("tests/test_a.py", reformatted, "test_b"); other != id {
		t.Fatal("formatter pass or added test changed the identity")
	}
	if _, err = redIdentity("tests/test_a.py", source, "test_c"); err == nil {
		t.Fatal("absent identifier accepted")
	}
	changed := []byte("def test_b():\n    assert answer() == 41\n")
	if checkFrozenRed("tests/test_a.py", source, reformatted, "test_b") != nil || checkFrozenRed("tests/test_a.py", source, changed, "test_b") == nil {
		t.Fatal("frozen declaration")
	}
}

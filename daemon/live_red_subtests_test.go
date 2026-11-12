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

// FAE-WF-036/073 (re-audit 9): a derived RED whose test function starts
// subtests with its own t.Run can never pass the RED gate, which needs
// exactly one executed test. Its RedReport is refused when the test-first
// turn completes, a ValidationFailed retry of the turn with the RED file put
// back, instead of a RED run that spends a test run and fails the gate.
// Subtests started some other way, t.Run in another function, and a
// shadowed t are not seen here; the RED gate still fails a RED that runs
// them. Needs git.
func TestLiveRedReportWithSubtestsRefusedAtTurnCompletion(t *testing.T) {
	ctx := context.Background()
	root := reviewPhysical(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	liveGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "answer.go"), []byte("package sample\n\nfunc answer() int { return 0 }\n"), 0600); err != nil {
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
	raw, err := json.Marshal(scopeEnvelope([]string{"answer.go", "answer_test.go"}))
	if err != nil {
		t.Fatal(err)
	}
	daemon.jobs["task"] = controlplane.JobSnapshot{ID: "task", Payload: raw}
	tree, err := x.broker.Tree(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	d := engine.Dispatch{TaskID: "task", Tree: tree, Config: defaults.Workflow().Stages["test-first"]}
	b := execution.Binding{Worktree: w.Path}
	report, _ := json.Marshal(map[string]any{"test_id": "TestBehavior", "test_shape": "go-unit", "declared_files": []string{"answer.go", "answer_test.go"}, "expected_failure": "got 0 want 42", "source": "subtests"})
	subtests := "package sample\n\nimport \"testing\"\n\nfunc TestBehavior(t *testing.T) {\n\tfor _, want := range []int{42} {\n\t\tt.Run(\"answer\", func(t *testing.T) {\n\t\t\tif answer() != want {\n\t\t\t\tt.Fatal(\"got 0 want 42\")\n\t\t\t}\n\t\t})\n\t}\n}\n"
	if err = os.WriteFile(filepath.Join(w.Path, "answer_test.go"), []byte(subtests), 0600); err != nil {
		t.Fatal(err)
	}
	err = x.checkRedReport(ctx, d, b, report)
	if !isScopeRefusal(err) || !strings.Contains(err.Error(), "RedReport refused at turn completion: RedReport test TestBehavior calls t.Run") {
		t.Fatalf("RED with subtests: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(w.Path, "answer_test.go")); !os.IsNotExist(statErr) {
		t.Fatal("refused RED turn not restored", statErr)
	}
	if err = os.WriteFile(filepath.Join(w.Path, "answer_test.go"), []byte(liveTestSource), 0600); err != nil {
		t.Fatal(err)
	}
	if err = x.checkRedReport(ctx, d, b, report); err != nil {
		t.Fatal("one test without subtests refused", err)
	}
}

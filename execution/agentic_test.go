package execution

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

func initTestGitRepo(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q")
	runGit("config", "user.name", "test")
	runGit("config", "user.email", "test@example.com")
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "initial")
	return runGit("rev-parse", "HEAD^{tree}")
}

type mockAgenticRuntime struct {
	run func(context.Context, runtime.Launch) (runtime.Collected, error)
}

func (m *mockAgenticRuntime) Run(ctx context.Context, l runtime.Launch) (runtime.Collected, error) {
	return m.run(ctx, l)
}

func TestExecutorAgenticOutOfScopeRevertedAndRefused(t *testing.T) {
	wt, _ := filepath.EvalSymlinks(t.TempDir())
	startTree := initTestGitRepo(t, wt, map[string]string{
		"in_scope.go": "package p\n",
		"readonly.md": "# Spec\n",
		"secret.txt":  "original\n",
	})
	logDir, _ := filepath.EvalSymlinks(t.TempDir())
	a := &admission{}
	role := workflow.Role{
		Instructions: "Implement",
		Routes:       []workflow.Binding{{Provider: "claude", Model: "model"}},
		Session:      "agentic",
		Profile:      workflow.Profile{Allowed: []string{"read", "edit", "write", "handoff"}},
	}
	doc := workflow.Document{
		ID:        "wf",
		Roles:     map[string]workflow.Role{"coder": role},
		Artifacts: map[string]workflow.ArtifactType{"output": {Schema: json.RawMessage(`{"type":"object"}`)}},
	}
	x := &Executor{
		Runtime: &mockAgenticRuntime{
			run: func(_ context.Context, l runtime.Launch) (runtime.Collected, error) {
				// Modify an out-of-scope file and a read-only file
				_ = os.WriteFile(filepath.Join(l.Worktree, "secret.txt"), []byte("tampered\n"), 0644)
				_ = os.WriteFile(filepath.Join(l.Worktree, "readonly.md"), []byte("modified\n"), 0644)
				_ = os.WriteFile(filepath.Join(l.Worktree, "untracked.txt"), []byte("rogue\n"), 0644)
				return runtime.Collected{
					ProcessesRetired: true,
					LogDirectory:     logDir,
					Output:           json.RawMessage(`{"pass":true}`),
				}, nil
			},
		},
		Resolve: func(context.Context, engine.Dispatch) (Binding, error) {
			return Binding{
				Worktree: wt,
				Mode:     "local",
				Document: doc,
				Scope:    TaskScope{Files: []string{"in_scope.go"}, ReadOnlyFiles: []string{"readonly.md"}},
			}, nil
		},
		Tree: func(ctx context.Context, b Binding) (string, error) {
			return provider.SnapshotWorktreeTree(ctx, b.Worktree)
		},
		Admission:   a,
		CheckChange: func(context.Context, engine.Dispatch, Binding) error { return nil },
	}
	d := engine.Dispatch{
		ExecutionKind: "live",
		ID:            "d-1",
		TaskID:        "task-1",
		Stage:         "implement",
		Tree:          startTree,
		Config:        workflow.Stage{Primitive: workflow.AgentTurn, Output: "output", Role: "coder", ChangeProducer: true, ReadOnlyFiles: []string{"readonly.md"}},
		Sessions:      []engine.Session{{ID: "s-1", Role: "coder"}},
	}
	r, err := x.Execute(context.Background(), d)
	if err == nil {
		t.Fatal("expected refusal error for out-of-scope modification")
	}
	if r.Class != engine.ClassMalformedOutput || !strings.Contains(r.Detail, "readonly.md") || !strings.Contains(r.Detail, "secret.txt") || !strings.Contains(r.Detail, "untracked.txt") {
		t.Fatalf("expected output-refused (ClassMalformedOutput) naming offending paths, got class=%q detail=%q err=%v", r.Class, r.Detail, err)
	}
	// Verify all out-of-scope edits were reverted
	if got, _ := os.ReadFile(filepath.Join(wt, "secret.txt")); string(got) != "original\n" {
		t.Fatalf("secret.txt was not reverted: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(wt, "readonly.md")); string(got) != "# Spec\n" {
		t.Fatalf("readonly.md was not reverted: %q", got)
	}
	if _, err := os.Stat(filepath.Join(wt, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked.txt was not cleaned: %v", err)
	}
}

func TestExecutorAgenticTurnBudgetExhaustionAndCheckpoint(t *testing.T) {
	wt, _ := filepath.EvalSymlinks(t.TempDir())
	startTree := initTestGitRepo(t, wt, map[string]string{
		"in_scope.go": "package p\n",
	})
	logDir, _ := filepath.EvalSymlinks(t.TempDir())
	a := &admission{}
	role := workflow.Role{
		Instructions: "Implement",
		Routes:       []workflow.Binding{{Provider: "claude", Model: "model"}},
		Session:      "agentic",
		Profile:      workflow.Profile{Allowed: []string{"read", "edit", "handoff"}},
		TurnBudget:   &workflow.TurnBudgetSpec{WallSeconds: 30, MaxAssistantTurns: 2, OnExhaustion: "resume-once"},
	}
	doc := workflow.Document{
		ID:        "wf",
		Roles:     map[string]workflow.Role{"coder": role},
		Artifacts: map[string]workflow.ArtifactType{"output": {Schema: json.RawMessage(`{"type":"object"}`)}},
	}

	var savedCP *engine.TurnCheckpoint
	var observedOutcome providerhealth.Outcome
	x := &Executor{
		Runtime: &mockAgenticRuntime{
			run: func(ctx context.Context, l runtime.Launch) (runtime.Collected, error) {
				_ = os.WriteFile(filepath.Join(l.Worktree, "in_scope.go"), []byte("package p\n// partial\n"), 0644)
				cpTree, _ := provider.SnapshotWorktreeTree(ctx, l.Worktree)
				_ = provider.SaveTurnCheckpointFile(logDir, runtime.TurnCheckpoint{
					ProviderSession: "sess-abc",
					WorktreeTree:    cpTree,
					AssistantTurns:  2,
				})
				return runtime.Collected{ProcessesRetired: true, LogDirectory: logDir}, &runtime.TurnError{
					Class: runtime.TurnBudgetExhausted,
					Cause: &runtime.BridgeRefusal{
						Class:  runtime.TurnBudgetExhausted,
						Detail: "max_assistant_turns exhausted",
					},
				}
			},
		},
		Resolve: func(context.Context, engine.Dispatch) (Binding, error) {
			return Binding{
				Worktree:              wt,
				Mode:                  "local",
				Document:              doc,
				Scope:                 TaskScope{Files: []string{"in_scope.go"}},
				InlineFileBytes:       2 << 20,
				InlineTurnBytes:       8 << 20,
				CheckpointIntervalSec: 60,
			}, nil
		},
		Tree: func(ctx context.Context, b Binding) (string, error) {
			return provider.SnapshotWorktreeTree(ctx, b.Worktree)
		},
		Admission:   a,
		CheckChange: func(context.Context, engine.Dispatch, Binding) error { return nil },
		SaveCheckpoint: func(_ context.Context, _ engine.Dispatch, cp engine.TurnCheckpoint) error {
			savedCP = &cp
			return nil
		},
		Observe: func(_ context.Context, _ engine.Dispatch, _ engine.Session, outcome providerhealth.Outcome) error {
			observedOutcome = outcome
			return nil
		},
	}
	d := engine.Dispatch{
		ExecutionKind: "live",
		ID:            "d-budget",
		TaskID:        "task-budget",
		Stage:         "implement",
		Visit:         1,
		Attempt:       1,
		Tree:          startTree,
		Config:        workflow.Stage{Primitive: workflow.AgentTurn, Output: "output", Role: "coder", ChangeProducer: true},
		Sessions:      []engine.Session{{ID: "s-1", Role: "coder"}},
	}
	r, err := x.Execute(context.Background(), d)
	if err == nil {
		t.Fatal("expected turn-budget-exhausted error from Execute")
	}
	if r.Class != engine.ClassTurnBudgetExhausted {
		t.Fatalf("expected class %q, got %q (%s)", engine.ClassTurnBudgetExhausted, r.Class, r.Detail)
	}
	if observedOutcome != providerhealth.TurnBudgetExhausted {
		t.Fatalf("expected provider health outcome %q, got %q", providerhealth.TurnBudgetExhausted, observedOutcome)
	}
	if savedCP == nil || savedCP.ProviderSession != "sess-abc" || r.Checkpoint == nil {
		t.Fatalf("expected checkpoint to be saved and attached to Result, savedCP=%+v r.Checkpoint=%+v", savedCP, r.Checkpoint)
	}

	// Simulate an interrupted worktree that also had an out-of-scope edit and an in-scope edit after the checkpoint:
	_ = os.WriteFile(filepath.Join(wt, "in_scope.go"), []byte("package p\n// partial + post-checkpoint\n"), 0644)
	_ = os.WriteFile(filepath.Join(wt, "out_of_scope.txt"), []byte("untracked out-of-scope\n"), 0644)

	var resumedSession string
	x.LoadCheckpoint = func(_ context.Context, _ engine.Dispatch) (*engine.TurnCheckpoint, error) {
		return savedCP, nil
	}
	x.Runtime = &mockAgenticRuntime{
		run: func(_ context.Context, l runtime.Launch) (runtime.Collected, error) {
			resumedSession = l.ResumeSession
			// Verify in-scope post-checkpoint edit was kept while out-of-scope file was cleaned before launch
			if got, _ := os.ReadFile(filepath.Join(l.Worktree, "in_scope.go")); !strings.Contains(string(got), "post-checkpoint") {
				t.Errorf("expected in-scope post-checkpoint edit kept, got %q", got)
			}
			if _, statErr := os.Stat(filepath.Join(l.Worktree, "out_of_scope.txt")); !os.IsNotExist(statErr) {
				t.Errorf("expected out_of_scope.txt cleaned before resume, got %v", statErr)
			}
			return runtime.Collected{
				ProcessesRetired: true,
				LogDirectory:     logDir,
				Output:           json.RawMessage(`{"pass":true}`),
			}, nil
		},
	}
	r2, err := x.Execute(context.Background(), d)
	if err != nil {
		t.Fatalf("Execute resume returned error: %v", err)
	}
	if resumedSession != "sess-abc" || !r2.ResumedFromCheckpoint {
		t.Fatalf("expected resume in sess-abc with ResumedFromCheckpoint=true, got session=%q resumed=%v", resumedSession, r2.ResumedFromCheckpoint)
	}
}

func TestExecutorForwardsRefusedReadsOnFailedTurnAndCheckpointsCrash(t *testing.T) {
	wt, _ := filepath.EvalSymlinks(t.TempDir())
	startTree := initTestGitRepo(t, wt, map[string]string{
		"in_scope.go": "package p\n",
	})
	logDir, _ := filepath.EvalSymlinks(t.TempDir())
	a := &admission{}
	role := workflow.Role{
		Instructions: "Implement",
		Routes:       []workflow.Binding{{Provider: "claude", Model: "model"}},
		Session:      "agentic",
		Profile:      workflow.Profile{Allowed: []string{"read", "edit", "handoff"}},
	}
	doc := workflow.Document{
		ID:        "wf",
		Roles:     map[string]workflow.Role{"coder": role},
		Artifacts: map[string]workflow.ArtifactType{"output": {Schema: json.RawMessage(`{"type":"object"}`)}},
	}

	var refusedPaths []string
	var savedCP *engine.TurnCheckpoint
	x := &Executor{
		Runtime: &mockAgenticRuntime{
			run: func(ctx context.Context, l runtime.Launch) (runtime.Collected, error) {
				subDir := filepath.Join(logDir, "sub")
				_ = os.MkdirAll(subDir, 0700)
				_ = os.WriteFile(filepath.Join(subDir, "refused-reads.jsonl"), []byte(`{"task":"task-crash","role":"coder","path":"secret.env"}`+"\n"), 0600)
				_ = os.WriteFile(filepath.Join(l.Worktree, "in_scope.go"), []byte("package p\n// crash edit\n"), 0644)
				cpTree, _ := provider.SnapshotWorktreeTree(ctx, l.Worktree)
				_ = provider.SaveTurnCheckpointFile(logDir, runtime.TurnCheckpoint{
					ProviderSession: "sess-crash-1",
					WorktreeTree:    cpTree,
					AssistantTurns:  1,
				})
				return runtime.Collected{ProcessesRetired: true, LogDirectory: logDir}, &runtime.TurnError{
					Class: "process-failed",
					Path:  filepath.Join(logDir, "handoff.json"),
				}
			},
		},
		Resolve: func(context.Context, engine.Dispatch) (Binding, error) {
			return Binding{
				Worktree: wt,
				Mode:     "local",
				Document: doc,
				Scope:    TaskScope{Files: []string{"in_scope.go"}},
				OnRefusedRead: func(_, _, p string) {
					refusedPaths = append(refusedPaths, p)
				},
			}, nil
		},
		Tree: func(ctx context.Context, b Binding) (string, error) {
			return provider.SnapshotWorktreeTree(ctx, b.Worktree)
		},
		Admission:   a,
		CheckChange: func(context.Context, engine.Dispatch, Binding) error { return nil },
		SaveCheckpoint: func(_ context.Context, _ engine.Dispatch, cp engine.TurnCheckpoint) error {
			savedCP = &cp
			return nil
		},
	}
	d := engine.Dispatch{
		ExecutionKind: "live",
		ID:            "d-crash",
		TaskID:        "task-crash",
		Stage:         "implement",
		Visit:         1,
		Attempt:       1,
		Tree:          startTree,
		Config:        workflow.Stage{Primitive: workflow.AgentTurn, Output: "output", Role: "coder", ChangeProducer: true},
		Sessions:      []engine.Session{{ID: "s-1", Role: "coder"}},
	}
	r, err := x.Execute(context.Background(), d)
	if err == nil {
		t.Fatal("expected error on crashed turn")
	}
	if len(refusedPaths) != 1 || refusedPaths[0] != "secret.env" {
		t.Fatalf("expected refused-reads forwarded on failed turn, got %v", refusedPaths)
	}
	if r.Class != engine.ClassCheckpointInterrupted {
		t.Fatalf("expected class %q, got %q", engine.ClassCheckpointInterrupted, r.Class)
	}
	if savedCP == nil || savedCP.ProviderSession != "sess-crash-1" || savedCP.WorktreeTree == startTree {
		t.Fatalf("expected checkpoint with in-scope edits saved before restoring startTree, got %+v", savedCP)
	}
}

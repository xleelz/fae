package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

func TestAutomaticConflictRefusesProtectedAndUnboundedBeforeEffects(t *testing.T) {
	x := &liveExecutor{config: LiveConfig{Files: []string{"answer.go", "answer_test.go"}, TestFile: "answer_test.go"}}
	for _, path := range []string{"answer_test.go", "outside.go"} {
		_, err := x.resolveConflict(context.Background(), engine.Dispatch{TaskID: "task", Config: workflow.Stage{Reverify: "loop"}}, worktree.Binding{}, effects.ReconcileEvidence{}, &effects.ReconcileError{Kind: "conflicted", Paths: []string{path}}, engine.Result{})
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("protected/undeclared path not refused before broker/store: %v", err)
		}
	}
	_, err := x.resolveConflict(context.Background(), engine.Dispatch{TaskID: "task"}, worktree.Binding{}, effects.ReconcileEvidence{}, &effects.ReconcileError{Kind: "conflicted", Paths: []string{"answer.go"}}, engine.Result{})
	if err == nil || !strings.Contains(err.Error(), "bounded") {
		t.Fatal("unbounded remediation accepted", err)
	}
}

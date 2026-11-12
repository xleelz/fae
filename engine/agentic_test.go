package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func agenticBudgetDoc(onExhaustion string) workflow.Document {
	pass := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "implement", Path: []string{"done"}}, {Op: "literal", Value: true}}}
	doc := workflow.Document{
		SchemaVersion: 1,
		ID:            "agentic-budget",
		Start:         "implement",
		Limits:        workflow.Limits{Attempts: 5, TestRuns: 2, ScopeWidens: 1},
		Roles: map[string]workflow.Role{
			"coder": {
				Instructions: "Implement the fix.",
				Briefs:       map[string]string{"default": "Fix {{task_id}}."},
				Routes:       []workflow.Binding{{Provider: "claude", Model: "model"}},
				Scope:        "task-bound",
				Cap:          1,
				Session:      "agentic",
				Profile:      workflow.Profile{Allowed: []string{"read", "edit", "handoff"}},
				TurnBudget:   &workflow.TurnBudgetSpec{WallSeconds: 60, MaxAssistantTurns: 3, OnExhaustion: onExhaustion},
				Outputs:      []string{"Report"},
			},
		},
		Artifacts: map[string]workflow.ArtifactType{
			"Report": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["done"],"additionalProperties":false,"properties":{"done":{"type":"boolean"}}}`)},
		},
		Stages: map[string]workflow.Stage{
			"implement": {Primitive: workflow.AgentTurn, Role: "coder", Output: "Report", ChangeProducer: true, ReadOnlyFiles: []string{"spec.md"}},
			"gate":      {Primitive: workflow.Gate, Expr: &pass, Failure: "stop"},
			"stop":      {Primitive: workflow.Escalate},
		},
		Edges: []workflow.Edge{
			{From: "implement", To: "gate"},
			{From: "gate", To: "Done"},
			{From: "gate", To: "stop"},
		},
	}
	if onExhaustion == "replan" {
		doc.Roles["planner"] = workflow.Role{
			Instructions: "Replan.",
			Briefs:       map[string]string{"default": "Replan {{task_id}}."},
			Routes:       []workflow.Binding{{Provider: "claude", Model: "model"}},
			Scope:        "task-bound",
			Cap:          1,
			Outputs:      []string{"Report"},
		}
		doc.Stages["replan"] = workflow.Stage{Primitive: workflow.AgentTurn, Role: "planner", Output: "Report"}
		doc.Edges = append(doc.Edges,
			workflow.Edge{From: "implement", To: "replan"},
			workflow.Edge{From: "replan", To: "Done"},
		)
	}
	return doc
}

func prepareNextDispatch(t *testing.T, j engine.Job) (engine.Job, *engine.Dispatch) {
	t.Helper()
	for i := 0; i < 5; i++ {
		next, d, _, err := engine.Prepare(j)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		j = next
		if d != nil || j.State.Terminal() || j.State == domain.Escalated || j.Parked {
			return j, d
		}
	}
	t.Fatalf("no dispatch produced from state %s", j.State)
	return j, nil
}

func TestTurnBudgetExhaustedResumeOnceAndCheckpointProvenance(t *testing.T) {
	doc := agenticBudgetDoc("resume-once")
	if issues := workflow.Validate(doc); len(issues) != 0 {
		t.Fatalf("validate: %v", issues)
	}
	j, err := engine.NewLive("task-resume", 1, doc, "tree-0")
	if err != nil {
		t.Fatal(err)
	}

	// First dispatch of implement
	j, d1 := prepareNextDispatch(t, j)
	if d1 == nil {
		t.Fatal("expected dispatch d1")
	}
	if len(d1.Config.ReadOnlyFiles) != 1 || d1.Config.ReadOnlyFiles[0] != "spec.md" {
		t.Fatalf("expected ReadOnlyFiles [spec.md], got %v", d1.Config.ReadOnlyFiles)
	}

	// Exhaust budget on first run with a checkpoint
	cp := &engine.TurnCheckpoint{
		TaskID:          "task-resume",
		StageID:         "implement",
		Visit:           d1.Visit,
		Attempt:         d1.Attempt,
		ProviderSession: "prov-sess-42",
		WorktreeTree:    "tree-partial",
		AssistantTurns:  3,
		UpdatedAt:       time.Now().UTC(),
	}
	j, events, err := engine.Complete(j, d1.ID, engine.Result{
		ExecutionKind: "live",
		DispatchID:    d1.ID,
		TaskID:        d1.TaskID,
		Session:       d1.Sessions[0].ID,
		Tree:          "tree-0",
		Class:         engine.ClassTurnBudgetExhausted,
		Detail:        "max_assistant_turns exhausted",
		Checkpoint:    cp,
		ProducedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Complete 1: %v", err)
	}
	hasExhausted := false
	for _, ev := range events {
		if ev.Kind == engine.ClassTurnBudgetExhausted {
			hasExhausted = true
		}
	}
	if !hasExhausted {
		t.Fatalf("expected turn-budget-exhausted event, got %+v", events)
	}
	if j.State != "implement" || j.Tree != "tree-0" {
		t.Fatalf("expected stage implement with tree-0, got state=%s tree=%s", j.State, j.Tree)
	}
	if j.TurnCheckpoints["implement"].WorktreeTree != "tree-partial" {
		t.Fatalf("expected checkpoint tree-partial, got %+v", j.TurnCheckpoints["implement"])
	}
	if j.Attempts != 0 {
		t.Fatalf("expected resume-once not to charge attempt budget, attempts=%d", j.Attempts)
	}

	// Second dispatch of implement should carry ResumeCheckpoint
	j, d2 := prepareNextDispatch(t, j)
	if d2 == nil {
		t.Fatal("expected dispatch d2")
	}
	if d2.ResumeCheckpoint == nil || d2.ResumeCheckpoint.ProviderSession != "prov-sess-42" || d2.ResumeCheckpoint.WorktreeTree != "tree-partial" {
		t.Fatalf("expected ResumeCheckpoint on second dispatch, got %+v", d2.ResumeCheckpoint)
	}

	// Complete second dispatch with handoff
	j, _, err = engine.Complete(j, d2.ID, engine.Result{
		ExecutionKind:         "live",
		DispatchID:            d2.ID,
		TaskID:                d2.TaskID,
		Session:               d2.Sessions[0].ID,
		Tree:                  "tree-0",
		NewTree:               "tree-final",
		Output:                json.RawMessage(`{"done":true}`),
		Checkpoint:            cp,
		ResumedFromCheckpoint: true,
		ProducedAt:            time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Complete 2: %v", err)
	}
	if len(j.Provenance) != 1 {
		t.Fatalf("expected 1 provenance record, got %d", len(j.Provenance))
	}
	prov := j.Provenance[0]
	if !prov.ResumedFromCheckpoint || prov.CheckpointSession != "prov-sess-42" || prov.CheckpointTree != "tree-partial" {
		t.Fatalf("unexpected provenance checkpoint metadata: %+v", prov)
	}

	// Advance gate -> Done
	j, _ = prepareNextDispatch(t, j)
	if j.State != domain.Done {
		t.Fatalf("expected Done after gate, state=%s", j.State)
	}
}

func TestTurnBudgetExhaustedTargetStageTransition(t *testing.T) {
	doc := agenticBudgetDoc("replan")
	j, err := engine.New("task-replan", 1, doc, "tree-0")
	if err != nil {
		t.Fatal(err)
	}
	j, d1 := prepareNextDispatch(t, j)
	if d1 == nil {
		t.Fatal("expected dispatch d1")
	}
	j, events, err := engine.Complete(j, d1.ID, engine.Result{
		DispatchID: d1.ID,
		TaskID:     d1.TaskID,
		Session:    d1.Sessions[0].ID,
		Tree:       "tree-0",
		Class:      engine.ClassTurnBudgetExhausted,
		Detail:     "wall_seconds exhausted",
		ProducedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if j.State != "replan" {
		t.Fatalf("expected transition to target stage replan, got %s (events=%+v)", j.State, events)
	}
}

func TestCheckpointInterruptedAndRecoverPreservesCheckpointWithoutParking(t *testing.T) {
	doc := agenticBudgetDoc("resume-once")
	j, err := engine.NewLive("task-cp-recover", 1, doc, "tree-0")
	if err != nil {
		t.Fatal(err)
	}
	j, d1 := prepareNextDispatch(t, j)
	if d1 == nil {
		t.Fatal("expected dispatch d1")
	}
	cp := &engine.TurnCheckpoint{
		TaskID:          "task-cp-recover",
		StageID:         "implement",
		Visit:           d1.Visit,
		Attempt:         d1.Attempt,
		ProviderSession: "sess-int-99",
		WorktreeTree:    "tree-mid",
		AssistantTurns:  2,
		UpdatedAt:       time.Now().UTC(),
	}
	j, events, err := engine.Complete(j, d1.ID, engine.Result{
		ExecutionKind: "live",
		DispatchID:    d1.ID,
		TaskID:        d1.TaskID,
		Session:       d1.Sessions[0].ID,
		Tree:          "tree-0",
		Class:         engine.ClassCheckpointInterrupted,
		Detail:        "interrupted mid-turn",
		Checkpoint:    cp,
		ProducedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Complete ClassCheckpointInterrupted: %v", err)
	}
	if j.Parked || j.InFlight != nil || j.Attempts != 0 {
		t.Fatalf("expected unparked refunded job after ClassCheckpointInterrupted, parked=%v inFlight=%v attempts=%d", j.Parked, j.InFlight, j.Attempts)
	}
	hasResumeEv := false
	for _, ev := range events {
		if ev.Kind == "checkpoint-resume" {
			hasResumeEv = true
		}
	}
	if !hasResumeEv {
		t.Fatalf("expected checkpoint-resume event, got %+v", events)
	}

	// Prepare next dispatch and simulate daemon crash recovery with checkpoint present.
	j, d2 := prepareNextDispatch(t, j)
	if d2 == nil || d2.ResumeCheckpoint == nil || d2.ResumeCheckpoint.ProviderSession != "sess-int-99" {
		t.Fatalf("expected d2 with ResumeCheckpoint sess-int-99, got %+v", d2)
	}
	recovered, _, err := engine.Recover(j, false, false)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if recovered.Parked || recovered.InFlight != nil || recovered.Attempts != 0 {
		t.Fatalf("expected Recover with checkpoint not to park UncertainDispatch, parked=%v reason=%s attempts=%d", recovered.Parked, recovered.Reason, recovered.Attempts)
	}
}

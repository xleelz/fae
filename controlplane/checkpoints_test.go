package controlplane

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestTurnCheckpointsRoundTrip(t *testing.T) {
	s, m := fixture(t, 2)
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)

	if err := s.SaveTurnCheckpoint(ctx, TurnCheckpointRecord{}); err == nil {
		t.Fatal("expected validation error for empty checkpoint")
	}

	cp := TurnCheckpointRecord{
		TaskID:          "task-1",
		StageID:         "implement",
		Visit:           1,
		Attempt:         2,
		ProviderSession: "session-123",
		WorktreeTree:    "deadbeef1234567890",
		AssistantTurns:  5,
		UpdatedAt:       now,
	}

	m.ExpectExec("CREATE TABLE IF NOT EXISTS turn_checkpoints").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectBegin()
	m.ExpectExec("REPLACE INTO turn_checkpoints").
		WithArgs("task-1", "implement", int64(1), int64(2), "session-123", "deadbeef1234567890", int64(5), now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()

	if err := s.SaveTurnCheckpoint(ctx, cp); err != nil {
		t.Fatalf("SaveTurnCheckpoint: %v", err)
	}

	m.ExpectExec("CREATE TABLE IF NOT EXISTS turn_checkpoints").WillReturnResult(sqlmock.NewResult(0, 0))
	rows := sqlmock.NewRows([]string{"task_id", "stage_id", "visit", "attempt", "provider_session", "worktree_tree", "assistant_turns", "updated_at"}).
		AddRow(cp.TaskID, cp.StageID, cp.Visit, cp.Attempt, cp.ProviderSession, cp.WorktreeTree, cp.AssistantTurns, cp.UpdatedAt)
	m.ExpectQuery("SELECT task_id,stage_id,visit,attempt,provider_session,worktree_tree,assistant_turns,updated_at FROM turn_checkpoints WHERE task_id=\\? AND stage_id=\\? AND visit=\\? AND attempt=\\?").
		WithArgs("task-1", "implement", int64(1), int64(2)).
		WillReturnRows(rows)

	got, ok, err := s.GetTurnCheckpoint(ctx, "task-1", "implement", 1, 2)
	if err != nil || !ok {
		t.Fatalf("GetTurnCheckpoint: ok=%v err=%v", ok, err)
	}
	if got.ProviderSession != cp.ProviderSession || got.WorktreeTree != cp.WorktreeTree || got.AssistantTurns != cp.AssistantTurns {
		t.Fatalf("GetTurnCheckpoint mismatch: got=%+v want=%+v", got, cp)
	}

	m.ExpectExec("CREATE TABLE IF NOT EXISTS turn_checkpoints").WillReturnResult(sqlmock.NewResult(0, 0))
	rowsLatest := sqlmock.NewRows([]string{"task_id", "stage_id", "visit", "attempt", "provider_session", "worktree_tree", "assistant_turns", "updated_at"}).
		AddRow(cp.TaskID, cp.StageID, cp.Visit, cp.Attempt, cp.ProviderSession, cp.WorktreeTree, cp.AssistantTurns, cp.UpdatedAt)
	m.ExpectQuery("SELECT task_id,stage_id,visit,attempt,provider_session,worktree_tree,assistant_turns,updated_at FROM turn_checkpoints WHERE task_id=\\? AND stage_id=\\? ORDER BY updated_at DESC").
		WithArgs("task-1", "implement").
		WillReturnRows(rowsLatest)

	latest, ok, err := s.LatestTurnCheckpoint(ctx, "task-1", "implement")
	if err != nil || !ok || latest.ProviderSession != "session-123" {
		t.Fatalf("LatestTurnCheckpoint: ok=%v err=%v latest=%+v", ok, err, latest)
	}

	m.ExpectExec("CREATE TABLE IF NOT EXISTS turn_checkpoints").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery("SELECT task_id,stage_id").
		WithArgs("task-1", "missing").
		WillReturnError(sql.ErrNoRows)
	_, ok, err = s.LatestTurnCheckpoint(ctx, "task-1", "missing")
	if err != nil || ok {
		t.Fatalf("expected not found for missing stage, got ok=%v err=%v", ok, err)
	}

	m.ExpectExec("CREATE TABLE IF NOT EXISTS turn_checkpoints").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectBegin()
	m.ExpectExec("DELETE FROM turn_checkpoints WHERE task_id=\\? AND stage_id=\\?").
		WithArgs("task-1", "implement").
		WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()

	if err := s.DeleteTurnCheckpoint(ctx, "task-1", "implement"); err != nil {
		t.Fatalf("DeleteTurnCheckpoint: %v", err)
	}
}

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// TurnCheckpointRecord is one durable agentic turn checkpoint row in
// turn_checkpoints (FAE-RT-019).
type TurnCheckpointRecord struct {
	TaskID          string    `json:"task_id"`
	StageID         string    `json:"stage_id"`
	Visit           int64     `json:"visit"`
	Attempt         int64     `json:"attempt"`
	ProviderSession string    `json:"provider_session"`
	WorktreeTree    string    `json:"worktree_tree"`
	AssistantTurns  int64     `json:"assistant_turns"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (s *Store) ensureTurnCheckpointsTable(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, TurnCheckpointsTableDDL)
	return err
}

// SaveTurnCheckpoint durably records or updates an agentic turn's
// {provider_session, worktree_tree} checkpoint in turn_checkpoints (FAE-RT-019).
func (s *Store) SaveTurnCheckpoint(ctx context.Context, cp TurnCheckpointRecord) error {
	if cp.TaskID == "" || cp.StageID == "" || cp.ProviderSession == "" || cp.WorktreeTree == "" || cp.Visit < 1 || cp.Attempt < 1 {
		return errors.New("turn checkpoint requires task_id, stage_id, visit>=1, attempt>=1, provider_session and worktree_tree")
	}
	if err := s.ensureTurnCheckpointsTable(ctx); err != nil {
		return err
	}
	now := cp.UpdatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return s.transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`REPLACE INTO turn_checkpoints (task_id,stage_id,visit,attempt,provider_session,worktree_tree,assistant_turns,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
			bounded(cp.TaskID, 255), bounded(cp.StageID, 255), cp.Visit, cp.Attempt, bounded(cp.ProviderSession, 255), bounded(cp.WorktreeTree, 255), cp.AssistantTurns, now,
		)
		return err
	})
}

// GetTurnCheckpoint reads the durable checkpoint for a specific
// (task_id, stage_id, visit, attempt), returning (record, false, nil) when
// none is recorded.
func (s *Store) GetTurnCheckpoint(ctx context.Context, taskID, stageID string, visit, attempt int64) (TurnCheckpointRecord, bool, error) {
	var cp TurnCheckpointRecord
	if taskID == "" || stageID == "" {
		return cp, false, errors.New("task_id and stage_id required")
	}
	if err := s.ensureTurnCheckpointsTable(ctx); err != nil {
		return cp, false, err
	}
	err := s.db.QueryRowContext(ctx,
		`SELECT task_id,stage_id,visit,attempt,provider_session,worktree_tree,assistant_turns,updated_at FROM turn_checkpoints WHERE task_id=? AND stage_id=? AND visit=? AND attempt=?`,
		taskID, stageID, visit, attempt,
	).Scan(&cp.TaskID, &cp.StageID, &cp.Visit, &cp.Attempt, &cp.ProviderSession, &cp.WorktreeTree, &cp.AssistantTurns, &cp.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return cp, false, nil
	}
	if err != nil {
		return cp, false, err
	}
	cp.UpdatedAt = cp.UpdatedAt.UTC()
	return cp, true, nil
}

// LatestTurnCheckpoint returns the most recently updated checkpoint for
// (task_id, stage_id), or (record, false, nil) if none exists.
func (s *Store) LatestTurnCheckpoint(ctx context.Context, taskID, stageID string) (TurnCheckpointRecord, bool, error) {
	var cp TurnCheckpointRecord
	if taskID == "" || stageID == "" {
		return cp, false, errors.New("task_id and stage_id required")
	}
	if err := s.ensureTurnCheckpointsTable(ctx); err != nil {
		return cp, false, err
	}
	err := s.db.QueryRowContext(ctx,
		`SELECT task_id,stage_id,visit,attempt,provider_session,worktree_tree,assistant_turns,updated_at FROM turn_checkpoints WHERE task_id=? AND stage_id=? ORDER BY updated_at DESC, visit DESC, attempt DESC LIMIT 1`,
		taskID, stageID,
	).Scan(&cp.TaskID, &cp.StageID, &cp.Visit, &cp.Attempt, &cp.ProviderSession, &cp.WorktreeTree, &cp.AssistantTurns, &cp.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return cp, false, nil
	}
	if err != nil {
		return cp, false, err
	}
	cp.UpdatedAt = cp.UpdatedAt.UTC()
	return cp, true, nil
}

// DeleteTurnCheckpoint removes durable checkpoints for (task_id, stage_id).
func (s *Store) DeleteTurnCheckpoint(ctx context.Context, taskID, stageID string) error {
	if taskID == "" || stageID == "" {
		return errors.New("task_id and stage_id required")
	}
	if err := s.ensureTurnCheckpointsTable(ctx); err != nil {
		return err
	}
	return s.transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM turn_checkpoints WHERE task_id=? AND stage_id=?`, taskID, stageID)
		return err
	})
}

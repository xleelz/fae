package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Migration11 lets the rows of finished tasks the drive does not materialise
// be read without their snapshots (FAE-CP-009). The task row already holds a
// Board row's run, priority, reason and escalation kind; migration11Columns
// adds the job's execution kind and the admission's mode, and
// backfillBoardColumns fills them for every finished task, whose row no later
// checkpoint rewrites. Other rows gain them at their next checkpoint.
var Migration11 = []string{}

// migration11Columns adds the Board columns, each only when absent.
func migration11Columns(ctx context.Context, tx *sql.Tx) error {
	for _, c := range []struct{ column, ddl string }{
		{"execution_kind", `ALTER TABLE tasks ADD COLUMN execution_kind VARCHAR(255) NULL`},
		{"mode", `ALTER TABLE tasks ADD COLUMN mode VARCHAR(255) NULL`},
	} {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='tasks' AND column_name=?`, c.column).Scan(&present); err != nil {
			return err
		}
		if present == 0 {
			if _, err := tx.ExecContext(ctx, c.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

// boardFacts are the snapshot fields a Board row takes beyond the job row.
type boardFacts struct {
	Job struct {
		ExecutionKind  string `json:"execution_kind"`
		Reason         string `json:"reason"`
		EscalationKind string `json:"escalation_kind"`
	} `json:"job"`
	Admission struct {
		RunID    string `json:"run_id"`
		Mode     string `json:"mode"`
		Priority int    `json:"priority"`
	} `json:"admission"`
}

// backfillBoardColumns projects, page by page in id order, each finished
// job's Board facts into its task row: the new columns and, rewritten from
// the same snapshot, the run, priority, reason and escalation kind. A row
// whose snapshot does not decode, or whose values the columns cannot hold
// exactly (BoardColumns), keeps NULL and is read from its snapshot. Replay
// fills only rows still NULL.
func (s *Store) backfillBoardColumns(ctx context.Context, tx *sql.Tx) error {
	type pending struct {
		id  string
		ref BlobRef
	}
	for cursor := ""; ; {
		rows, err := tx.QueryContext(ctx, `SELECT j.job_id,j.payload_digest,j.payload_size FROM jobs j JOIN tasks t ON t.task_id=j.job_id WHERE j.state IN ('Cancelled','Done','Retired') AND j.job_id>? AND t.execution_kind IS NULL ORDER BY j.job_id LIMIT 1000`, cursor)
		if err != nil {
			return err
		}
		page := []pending{}
		for rows.Next() {
			var p pending
			if err = rows.Scan(&p.id, &p.ref.Digest, &p.ref.Size); err != nil {
				rows.Close()
				return err
			}
			page = append(page, p)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, p := range page {
			cursor = p.id
			raw, err := getBlob(ctx, tx, p.ref)
			if err != nil {
				return err
			}
			var f boardFacts
			if json.Unmarshal(raw, &f) != nil {
				continue
			}
			kind, mode := BoardColumns(f.Job.ExecutionKind, f.Admission.Mode, f.Admission.RunID, f.Admission.Priority)
			if kind == nil {
				continue
			}
			reason, err := s.inlineOrRef(ctx, tx, []byte(f.Job.Reason))
			if err != nil {
				return err
			}
			var escalation any
			if f.Job.EscalationKind != "" {
				escalation = f.Job.EscalationKind
			}
			if _, err = tx.ExecContext(ctx, `UPDATE tasks SET execution_kind=?,mode=?,run_id=?,priority=?,escalation_kind=?,escalation_reason=? WHERE task_id=?`, kind, mode, f.Admission.RunID, f.Admission.Priority, escalation, reason, p.id); err != nil {
				return err
			}
		}
		if len(page) < 1000 {
			return nil
		}
	}
}

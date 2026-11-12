package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// MigrateJob is SaveJob's version CAS for the one binding change SaveJob
// forbids: an explicit operator move to another existing revision of the same,
// unretired workflow. The snapshot, events and audit row commit together.
func (s *Store) MigrateJob(ctx context.Context, a Audit, j JobSnapshot, expected, fromRevision int64) (JobSnapshot, error) {
	if e := validateJob(j); e != nil {
		return j, e
	}
	if expected < 1 || j.WorkflowRevision == fromRevision {
		return j, ErrConflict
	}
	j.Version = expected + 1
	e := s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if e := lockEvents(ctx, tx); e != nil {
			return e
		}
		old, _, e := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE job_id=? FOR UPDATE`, j.ID))
		if e != nil {
			return e
		}
		if old.Version != expected || old.WorkflowID != j.WorkflowID || old.WorkflowRevision != fromRevision || old.TargetID != j.TargetID || old.TargetRevision != j.TargetRevision || old.State == "Done" || old.State == "Retired" || old.State == "Cancelled" {
			return ErrConflict
		}
		var revisions, retired int
		if e = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflows WHERE workflow_id=? AND revision=?`, j.WorkflowID, j.WorkflowRevision).Scan(&revisions); e != nil {
			return e
		}
		if e = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_status WHERE workflow_id=? AND retired_at IS NOT NULL`, j.WorkflowID).Scan(&retired); e != nil {
			return e
		}
		if revisions != 1 || retired != 0 {
			return errors.New("migration target revision is unknown or its workflow is retired")
		}
		// The moved task joins the target revision's molecule; the old one ends
		// once it holds no other live task.
		if j.MoleculeID, e = s.bindMoleculeTx(ctx, tx, j.WorkflowID, j.WorkflowRevision, true); e != nil {
			return e
		}
		payload, e := CanonicalJSON(j.Payload)
		if e != nil {
			return e
		}
		if e = s.putBlob(ctx, tx, payload); e != nil {
			return e
		}
		result, e := tx.ExecContext(ctx, `UPDATE jobs SET version=?,workflow_revision=?,state=?,payload_digest=?,payload_size=?,molecule_id=? WHERE job_id=? AND version=?`, j.Version, j.WorkflowRevision, j.State, Digest(payload), len(payload), j.MoleculeID, j.ID, expected)
		if e != nil {
			return e
		}
		if n, e := result.RowsAffected(); e != nil || n != 1 {
			return errors.Join(ErrConflict, e)
		}
		if e = s.recordJobTx(ctx, tx, a, j, j.MoleculeID); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE backlog_items SET state=?,updated_at=? WHERE item_id=?`, j.State, time.Now().UTC(), j.ID); e != nil {
			return e
		}
		return s.appendEvents(ctx, tx, j.ID, j.Events, "job_migrated")
	})
	return j, e
}

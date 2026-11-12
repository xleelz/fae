package controlplane

import (
	"context"
	"database/sql"
	"errors"
)

// immutable makes a record table append-only in the plane itself: an UPDATE,
// a DELETE or a REPLACE of an existing key is refused by a trigger, whoever
// issues it (FAE-WF-051). key is the NEW-row predicate naming an existing row.
func immutable(table, key string) []string {
	signal := func(text string) string {
		return `BEGIN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '` + table + ` ` + text + `'; END`
	}
	return []string{
		`DROP TRIGGER IF EXISTS ` + table + `_no_update`,
		`CREATE TRIGGER ` + table + `_no_update BEFORE UPDATE ON ` + table + ` FOR EACH ROW ` + signal("rows are immutable"),
		`DROP TRIGGER IF EXISTS ` + table + `_no_delete`,
		`CREATE TRIGGER ` + table + `_no_delete BEFORE DELETE ON ` + table + ` FOR EACH ROW ` + signal("rows are append-only"),
		`DROP TRIGGER IF EXISTS ` + table + `_no_replace`,
		`CREATE TRIGGER ` + table + `_no_replace BEFORE INSERT ON ` + table + ` FOR EACH ROW BEGIN IF EXISTS (SELECT 1 FROM ` + table + ` WHERE ` + key + `) THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = '` + table + ` rows are immutable'; END IF; END`,
	}
}

// Migration8 makes the relational task records authoritative (FAE-CP-011,
// FAE-WF-006, -022, -051): an append-only provenance table with the session
// and transcript reference, and immutability triggers on provenance,
// artifacts, transitions and decisions. migration8Columns and backfillRecords
// complete it in the same transaction.
var Migration8 = append([]string{
	`CREATE INDEX IF NOT EXISTS molecules_running ON molecules (ended_at,molecule_id)`,
	`CREATE TABLE IF NOT EXISTS provenance (provenance_id CHAR(64) PRIMARY KEY, artifact_id CHAR(64) NOT NULL, task_id VARCHAR(255) NOT NULL, molecule_id VARCHAR(64) NOT NULL, kind VARCHAR(255) NOT NULL, dispatch_id VARCHAR(1024) NOT NULL, stage VARCHAR(255) NOT NULL, primitive VARCHAR(64) NOT NULL, role VARCHAR(255) NOT NULL, provider VARCHAR(255) NOT NULL, model VARCHAR(255) NOT NULL, session_id VARCHAR(1024) NOT NULL, transcript TEXT NOT NULL, tree_hash VARCHAR(255) NOT NULL, inputs TEXT NOT NULL, commit_ref VARCHAR(255) NOT NULL, execution_kind VARCHAR(64) NOT NULL, workflow_digest VARCHAR(64) NOT NULL, workflow_revision BIGINT NOT NULL, produced_at DATETIME(6) NOT NULL, created_at DATETIME(6) NOT NULL, INDEX provenance_artifact(task_id,artifact_id,produced_at), INDEX provenance_commit(commit_ref), INDEX provenance_task(task_id,produced_at))`,
}, append(append(append(immutable("provenance", "provenance_id=NEW.provenance_id"), immutable("artifacts", "artifact_id=NEW.artifact_id AND task_id=NEW.task_id")...), immutable("transitions", "id=NEW.id")...), immutable("decisions", "id=NEW.id")...)...)

// migration8Columns adds the job's molecule and the task row's record cursors.
// Dolt has no ADD COLUMN IF NOT EXISTS, so each is added only when absent.
func migration8Columns(ctx context.Context, tx *sql.Tx) error {
	for _, c := range []struct{ table, column, ddl string }{
		{"jobs", "molecule_id", `ALTER TABLE jobs ADD COLUMN molecule_id CHAR(64) NOT NULL DEFAULT ''`},
		{"tasks", "history_cursor", `ALTER TABLE tasks ADD COLUMN history_cursor BIGINT NOT NULL DEFAULT 0`},
		{"tasks", "provenance_cursor", `ALTER TABLE tasks ADD COLUMN provenance_cursor BIGINT NOT NULL DEFAULT 0`},
	} {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`, c.table, c.column).Scan(&present); err != nil {
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

// backfillRecords binds every existing job to its revision's molecule and
// projects its task records, in the migration's transaction. Molecules bound
// under the earlier identity (the bare content hash, which collided for
// revisions with identical content) take the resolved identity first. A job
// whose payload is not a task envelope, or whose revision is unknown, is left
// unbound: the daemon quarantines an invalid job at load.
func (s *Store) backfillRecords(ctx context.Context, tx *sql.Tx) error {
	type molecule struct {
		id, workflow, digest string
		revision             int64
	}
	rows, err := tx.QueryContext(ctx, `SELECT molecule_id,workflow_id,revision,content_digest FROM molecules ORDER BY molecule_id LIMIT 100001`)
	if err != nil {
		return err
	}
	legacy := []molecule{}
	for rows.Next() {
		var m molecule
		if err = rows.Scan(&m.id, &m.workflow, &m.revision, &m.digest); err != nil {
			rows.Close()
			return err
		}
		legacy = append(legacy, m)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, m := range legacy {
		id, err := MoleculeID(m.workflow, m.revision, m.digest)
		if err != nil || id == m.id || m.id != m.digest {
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE molecules SET molecule_id=? WHERE molecule_id=?`, id, m.id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE tasks SET molecule_id=? WHERE molecule_id=?`, id, m.id); err != nil {
			return err
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT job_id FROM jobs WHERE molecule_id='' ORDER BY job_id LIMIT 100001`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(ids) > 100000 {
		return errors.New("job backfill bound exceeded")
	}
	for _, id := range ids {
		if err = s.inject("backfill"); err != nil {
			return err
		}
		j, ref, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE job_id=? FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if j.Payload, err = getBlob(ctx, tx, ref); err != nil {
			return err
		}
		e, err := decodeTaskEnvelope(j)
		if err != nil {
			continue
		}
		molecule, err := s.bindMoleculeTx(ctx, tx, j.WorkflowID, j.WorkflowRevision, false)
		if errors.Is(err, ErrUnknownRevision) || errors.Is(err, ErrMoleculeMismatch) {
			continue
		}
		if err != nil {
			return err
		}
		c, err := readTaskCursor(ctx, tx, j.ID)
		if err != nil {
			return err
		}
		plan, err := planRecords(j, e.Job, c, molecule, "migration")
		if err != nil {
			// Legacy history the record rules cannot project stays in the
			// snapshot: the task row starts its cursors after it, so the
			// task continues and every later transition is recorded.
			plan = recordPlan{}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE jobs SET molecule_id=? WHERE job_id=?`, molecule, j.ID); err != nil {
			return err
		}
		if err = s.writeRecordsTx(ctx, tx, Audit{Actor: "migration"}, j, e, c, plan, molecule, false); err != nil {
			return err
		}
	}
	return nil
}

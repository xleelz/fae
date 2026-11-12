package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/xleelz/fae/engine"
)

// Migration9 gives every FAE-CP-011 record family a production writer. The
// ticket outbox was never written before it, so it is rebuilt keyed by the
// emission (a task has one row per lifecycle step); migration9Columns adds
// the columns the projections need and backfillFamilies projects the
// existing authoritative state, all in the migration's transaction.
var Migration9 = []string{
	`DROP TABLE IF EXISTS ticket_outbox`,
	`CREATE TABLE IF NOT EXISTS ticket_outbox (outbox_key CHAR(64) PRIMARY KEY, task_id VARCHAR(255) NOT NULL, room VARCHAR(255) NOT NULL, position BIGINT NOT NULL, ticket_id VARCHAR(255) NOT NULL, desired_step VARCHAR(255) NOT NULL, started BOOLEAN NOT NULL, emitted BOOLEAN NOT NULL, attempts BIGINT NOT NULL, last_error_digest CHAR(64), updated_at DATETIME(6) NOT NULL, INDEX outbox_task(task_id,room,position))`,
	`CREATE INDEX IF NOT EXISTS reviews_task ON code_reviews (task_id)`,
	`CREATE INDEX IF NOT EXISTS engine_runs_task ON engine_runs (task_id,started_at)`,
	`CREATE INDEX IF NOT EXISTS effects_task ON external_effects (task_id,created_at)`,
	`CREATE INDEX IF NOT EXISTS sessions_task ON sessions (task_id,launched_at)`,
	`CREATE INDEX IF NOT EXISTS leases_open ON leases (released_at,scope_key)`,
	`CREATE INDEX IF NOT EXISTS backlog_state ON backlog_items (state,item_id)`,
	`CREATE INDEX IF NOT EXISTS backlog_dependents ON backlog_edges (to_id,kind)`,
	`CREATE INDEX IF NOT EXISTS outbox_pending ON ticket_outbox (emitted,task_id)`,
}

// migration9Columns adds the projection columns, each only when absent, and
// makes the session process columns nullable: a provenance entry names a
// session, not its process tree.
func migration9Columns(ctx context.Context, tx *sql.Tx) error {
	for _, c := range []struct{ table, column, ddl string }{
		{"external_effects", "session_id", `ALTER TABLE external_effects ADD COLUMN session_id VARCHAR(1024) NOT NULL DEFAULT ''`},
		{"external_effects", "outcome", `ALTER TABLE external_effects ADD COLUMN outcome VARCHAR(64) NOT NULL DEFAULT ''`},
		{"external_effects", "updated_at", `ALTER TABLE external_effects ADD COLUMN updated_at DATETIME(6) NULL`},
		{"schedules", "marker", `ALTER TABLE schedules ADD COLUMN marker VARCHAR(255) NOT NULL DEFAULT ''`},
		{"schedules", "updated_at", `ALTER TABLE schedules ADD COLUMN updated_at DATETIME(6) NULL`},
		{"engine_runs", "dispatch_id", `ALTER TABLE engine_runs ADD COLUMN dispatch_id VARCHAR(1024) NOT NULL DEFAULT ''`},
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
	var required int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='sessions' AND column_name IN ('owner_pid','owner_start','process_root') AND is_nullable='NO'`).Scan(&required); err != nil {
		return err
	}
	if required > 0 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE sessions MODIFY COLUMN owner_pid BIGINT NULL, MODIFY COLUMN owner_start VARCHAR(255) NULL, MODIFY COLUMN process_root BIGINT NULL`); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS schedules_marker ON schedules (marker,schedule_id)`)
	return err
}

// backfillFamilies projects the state that existed before migration 9: open
// leases, ticket emissions, schedule markers, effect checkpoints, sessions
// named by provenance and reviews named by task rows.
func (s *Store) backfillFamilies(ctx context.Context, tx *sql.Tx) error {
	now := time.Now().UTC()
	type keyed struct{ key, kind, payload string }
	pools, err := collect(ctx, tx, `SELECT pool_key,kind,payload FROM resource_pools ORDER BY pool_key LIMIT 100001`, func(r *sql.Rows) (keyed, error) {
		var k keyed
		return k, r.Scan(&k.key, &k.kind, &k.payload)
	})
	if err != nil {
		return err
	}
	for _, k := range pools {
		p, err := decodePool(ctx, tx, k.payload)
		if err != nil {
			continue // an unreadable ledger fails closed at its next use
		}
		if err = projectLeasesTx(ctx, tx, k.key, k.kind, nil, p.Holders, nil, "", now); err != nil {
			return err
		}
	}
	emissions, err := collect(ctx, tx, `SELECT payload FROM ticket_emissions ORDER BY outbox_key LIMIT 100001`, func(r *sql.Rows) (string, error) {
		var raw string
		return raw, r.Scan(&raw)
	})
	if err != nil {
		return err
	}
	for _, raw := range emissions {
		var t TicketEmission
		if json.Unmarshal([]byte(raw), &t) != nil {
			continue
		}
		if err = s.upsertOutboxTx(ctx, tx, t, now); err != nil {
			return err
		}
	}
	type marker struct {
		name string
		ref  BlobRef
	}
	scanMarker := func(r *sql.Rows) (marker, error) {
		var m marker
		return m, r.Scan(&m.name, &m.ref.Digest, &m.ref.Size)
	}
	for _, query := range []struct {
		sql   string
		apply func(string, []byte) error
	}{
		{`SELECT name,payload_digest,payload_size FROM daemon_markers WHERE name IN ('cron-schedules','provider-loops','backup-creation-schedule','backup-retention-schedule','backup-replication-schedule') ORDER BY name`, func(name string, raw []byte) error {
			return s.projectSchedulesTx(ctx, tx, Audit{Actor: "migration", Action: "migration 9"}, name, raw)
		}},
		{`SELECT name,payload_digest,payload_size FROM daemon_markers WHERE name LIKE '%/external-intent' ORDER BY name LIMIT 100001`, func(_ string, raw []byte) error {
			if e, ok := effectFromCheckpoint("intent", raw); ok {
				return s.upsertEffectTx(ctx, tx, e, now)
			}
			return nil
		}},
		{`SELECT name,payload_digest,payload_size FROM daemon_markers WHERE name LIKE '%/observed/%' OR name LIKE 'ticket-observed/%' ORDER BY name LIMIT 100001`, func(_ string, raw []byte) error {
			if e, ok := effectFromCheckpoint("observed", raw); ok {
				return s.upsertEffectTx(ctx, tx, e, now)
			}
			return nil
		}},
	} {
		markers, err := collect(ctx, tx, query.sql, scanMarker)
		if err != nil {
			return err
		}
		for _, m := range markers {
			raw, err := getBlob(ctx, tx, m.ref)
			if err != nil {
				return err
			}
			if err = query.apply(m.name, raw); err != nil {
				return err
			}
		}
	}
	type sessionRow struct {
		r          ProvenanceRecord
		transcript string
	}
	sessions, err := collect(ctx, tx, `SELECT session_id,task_id,role,provider,model,execution_kind,stage,transcript,produced_at FROM provenance WHERE session_id<>'' ORDER BY produced_at LIMIT 100001`, func(r *sql.Rows) (sessionRow, error) {
		var x sessionRow
		return x, r.Scan(&x.r.SessionID, &x.r.TaskID, &x.r.Role, &x.r.Provider, &x.r.Model, &x.r.ExecutionKind, &x.r.Stage, &x.transcript, &x.r.ProducedAt)
	})
	if err != nil {
		return err
	}
	for _, x := range sessions {
		if x.transcript != "" {
			x.r.Transcript = &engine.Transcript{}
			_ = json.Unmarshal([]byte(x.transcript), x.r.Transcript)
		}
		if err = s.upsertSessionTx(ctx, tx, x.r); err != nil {
			return err
		}
	}
	reviews, err := collect(ctx, tx, `SELECT task_id FROM tasks WHERE review_ref IS NOT NULL ORDER BY task_id LIMIT 100001`, func(r *sql.Rows) (string, error) {
		var id string
		return id, r.Scan(&id)
	})
	if err != nil {
		return err
	}
	for _, task := range reviews {
		var ref BlobRef
		err := tx.QueryRowContext(ctx, `SELECT payload_digest,payload_size FROM daemon_markers WHERE name=?`, ReviewMarker(task)).Scan(&ref.Digest, &ref.Size)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		raw, err := getBlob(ctx, tx, ref)
		if err != nil {
			return err
		}
		var c struct {
			ID                string `json:"id"`
			Revision          int    `json:"revision"`
			Published, Merged bool
		}
		if json.Unmarshal(raw, &c) != nil {
			continue
		}
		if err = (CodeReview{ID: c.ID, Task: task, Revisions: c.Revision, Published: c.Published, Merged: c.Merged}).projectTx(ctx, s, tx, raw, now); err != nil {
			return err
		}
	}
	return nil
}

// ReviewMarker is the marker name of a task's review lifecycle.
func ReviewMarker(task string) string {
	raw, _ := json.Marshal(task)
	h, _, _ := ContentHash(raw)
	return "review-lifecycle/" + h
}

// collect reads a bounded result set into memory before the next statement.
func collect[T any](ctx context.Context, tx *sql.Tx, query string, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, v)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(out) > 100000 {
		return nil, errors.New("migration backfill bound exceeded")
	}
	return out, nil
}

package controlplane

import (
	"context"
	"database/sql"
	"fmt"
)

const SchemaVersion = 12

// SchemaStatements is forward-only migration 1. SQL uses Dolt's MySQL dialect.
var SchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS audit (id BIGINT AUTO_INCREMENT PRIMARY KEY, actor VARCHAR(255) NOT NULL, action VARCHAR(255) NOT NULL, message_id VARCHAR(255) NOT NULL, inputs TEXT NOT NULL, argument_digest CHAR(64) NOT NULL, outcome TEXT NOT NULL, created_at DATETIME(6) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS workflows (workflow_id VARCHAR(255) NOT NULL, revision BIGINT NOT NULL, parent_revision BIGINT NULL, name VARCHAR(255) NOT NULL, content_digest CHAR(64) NOT NULL, created_by VARCHAR(255) NOT NULL, created_at DATETIME(6) NOT NULL, PRIMARY KEY(workflow_id,revision), CHECK(revision>=1))`,
	`CREATE TABLE IF NOT EXISTS workflow_status (workflow_id VARCHAR(255) PRIMARY KEY, retired_at DATETIME(6), retired_by VARCHAR(255), reason TEXT)`,
	`CREATE TABLE IF NOT EXISTS molecules (molecule_id CHAR(64) PRIMARY KEY, workflow_id VARCHAR(255) NOT NULL, revision BIGINT NOT NULL, content_digest CHAR(64) NOT NULL, created_at DATETIME(6) NOT NULL, ended_at DATETIME(6), UNIQUE KEY workflow_revision(workflow_id,revision))`,
	`CREATE TABLE IF NOT EXISTS target_configurations (id VARCHAR(255) NOT NULL, revision BIGINT NOT NULL, content_digest CHAR(64) NOT NULL, created_at DATETIME(6) NOT NULL, PRIMARY KEY(id,revision))`,
	`CREATE TABLE IF NOT EXISTS tasks (task_id VARCHAR(255) PRIMARY KEY, run_id VARCHAR(255) NOT NULL, molecule_id CHAR(64) NOT NULL, target_id VARCHAR(255) NOT NULL, target_revision BIGINT NOT NULL, state VARCHAR(255) NOT NULL, attempt BIGINT NOT NULL DEFAULT 0, attempt_limit BIGINT NOT NULL DEFAULT 20, priority BIGINT NOT NULL DEFAULT 10, disposition TEXT NOT NULL, escalation_kind VARCHAR(255), escalation_reason TEXT, commit_ref VARCHAR(255), review_ref VARCHAR(255), created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL, INDEX task_run(run_id,state), INDEX task_molecule(molecule_id,state), CHECK(attempt>=0), CHECK(attempt_limit>0), CHECK(priority>=10))`,
	`CREATE TABLE IF NOT EXISTS backlog_items (item_id VARCHAR(255) PRIMARY KEY, source_ref VARCHAR(255), priority BIGINT NOT NULL, acceptance_digest CHAR(64) NOT NULL, write_class TEXT, state VARCHAR(255) NOT NULL, deferred_until DATETIME(6), created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL, CHECK(priority>=10))`,
	`CREATE TABLE IF NOT EXISTS backlog_edges (from_id VARCHAR(255) NOT NULL, to_id VARCHAR(255) NOT NULL, kind VARCHAR(32) NOT NULL, created_by VARCHAR(255) NOT NULL, created_at DATETIME(6) NOT NULL, PRIMARY KEY(from_id,to_id,kind), CHECK(kind IN ('blocking','relates')))`,
	`CREATE TABLE IF NOT EXISTS transitions (id BIGINT AUTO_INCREMENT PRIMARY KEY, task_id VARCHAR(255) NOT NULL, from_state VARCHAR(255) NOT NULL, to_state VARCHAR(255) NOT NULL, owner VARCHAR(255) NOT NULL, attempt BIGINT NOT NULL, evidence_digest CHAR(64) NOT NULL, note TEXT NOT NULL, message_id VARCHAR(255) NOT NULL, created_at DATETIME(6) NOT NULL, INDEX task_history(task_id,id))`,
	`CREATE TABLE IF NOT EXISTS decisions (id VARCHAR(255) PRIMARY KEY, task_id VARCHAR(255) NOT NULL, kind VARCHAR(255) NOT NULL, payload_digest CHAR(64) NOT NULL, rationale TEXT, role VARCHAR(255), session_id VARCHAR(255), supersedes_id VARCHAR(255), created_at DATETIME(6) NOT NULL, INDEX task_decisions(task_id,created_at))`,
	`CREATE TABLE IF NOT EXISTS artifacts (artifact_id CHAR(64) NOT NULL, task_id VARCHAR(255) NOT NULL, kind VARCHAR(255) NOT NULL, schema_id VARCHAR(255) NOT NULL, schema_version BIGINT NOT NULL, content_digest CHAR(64) NOT NULL, provenance TEXT NOT NULL, created_at DATETIME(6) NOT NULL, PRIMARY KEY(artifact_id,task_id), INDEX task_artifacts(task_id,kind))`,
	`CREATE TABLE IF NOT EXISTS blob_chunks (digest CHAR(64) NOT NULL, chunk_index BIGINT NOT NULL, data BLOB NOT NULL, PRIMARY KEY(digest,chunk_index))`,
	`CREATE TABLE IF NOT EXISTS blobs (digest CHAR(64) PRIMARY KEY, size BIGINT NOT NULL, chunks BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS leases (lease_id VARCHAR(255) PRIMARY KEY, kind VARCHAR(64) NOT NULL, scope_key VARCHAR(255) NOT NULL, owner VARCHAR(255) NOT NULL, weight BIGINT NOT NULL, priority_class VARCHAR(64) NOT NULL, granted_at DATETIME(6) NOT NULL, expires_at DATETIME(6) NOT NULL, released_at DATETIME(6), release_reason TEXT, INDEX lease_scope(scope_key,released_at), CHECK(weight>0))`,
	`CREATE TABLE IF NOT EXISTS sessions (session_id VARCHAR(255) PRIMARY KEY, pane_id VARCHAR(255), role VARCHAR(255) NOT NULL, task_id VARCHAR(255), binding TEXT NOT NULL, owner_pid BIGINT NOT NULL, owner_start VARCHAR(255) NOT NULL, launched_at DATETIME(6) NOT NULL, reaped_at DATETIME(6), process_root BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS provider_health (provider VARCHAR(255) NOT NULL, model VARCHAR(255) NOT NULL, outcome VARCHAR(64), score DOUBLE NOT NULL, failures BIGINT NOT NULL, last_ok DATETIME(6), last_failure DATETIME(6), hold_until DATETIME(6), PRIMARY KEY(provider,model))`,
	`CREATE TABLE IF NOT EXISTS operator_messages (message_id VARCHAR(255) PRIMARY KEY, thread_root VARCHAR(255) NOT NULL, author VARCHAR(255) NOT NULL, text_digest CHAR(64) NOT NULL, created_at DATETIME(6) NOT NULL, consumed_by TEXT)`,
	`CREATE TABLE IF NOT EXISTS external_effects (effect_id VARCHAR(255) PRIMARY KEY, task_id VARCHAR(255) NOT NULL, tool VARCHAR(255) NOT NULL, argument_digest CHAR(64) NOT NULL, pinned_values TEXT NOT NULL, external_id VARCHAR(255), reconciliation VARCHAR(64) NOT NULL, created_at DATETIME(6) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS code_reviews (review_id VARCHAR(255) PRIMARY KEY, task_id VARCHAR(255) NOT NULL, branch VARCHAR(255) NOT NULL, tree_hash VARCHAR(255) NOT NULL, revisions BIGINT NOT NULL, publish_state VARCHAR(64) NOT NULL, merge_state VARCHAR(64) NOT NULL, status_digest CHAR(64))`,
	`CREATE TABLE IF NOT EXISTS ticket_outbox (task_id VARCHAR(255) PRIMARY KEY, ticket_id VARCHAR(255) NOT NULL, desired_step VARCHAR(255) NOT NULL, emitted BOOLEAN NOT NULL, attempts BIGINT NOT NULL, last_error_digest CHAR(64))`,
	`CREATE TABLE IF NOT EXISTS schedules (schedule_id VARCHAR(255) PRIMARY KEY, kind VARCHAR(64) NOT NULL, specification TEXT NOT NULL, state VARCHAR(64) NOT NULL, provenance TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS engine_runs (run_id VARCHAR(255) PRIMARY KEY, task_id VARCHAR(255) NOT NULL, command TEXT NOT NULL, tree_hash VARCHAR(255) NOT NULL, environment_delta TEXT NOT NULL, exit_code BIGINT NOT NULL, executed_count BIGINT NOT NULL, output_digest CHAR(64) NOT NULL, started_at DATETIME(6) NOT NULL, finished_at DATETIME(6) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS alerts (id BIGINT AUTO_INCREMENT PRIMARY KEY, severity VARCHAR(64) NOT NULL, source VARCHAR(255) NOT NULL, text_digest CHAR(64) NOT NULL, created_at DATETIME(6) NOT NULL, acknowledged_at DATETIME(6))`,
}

// Migration2 stores engine snapshots by content reference; event_clock serializes
// event writers so sequence order matches commit order, even with concurrent RPCs.
var Migration2 = []string{
	`CREATE TABLE IF NOT EXISTS jobs (job_id VARCHAR(255) PRIMARY KEY, version BIGINT NOT NULL, workflow_id VARCHAR(255) NOT NULL, workflow_revision BIGINT NOT NULL, target_id VARCHAR(255) NOT NULL, target_revision BIGINT NOT NULL, state VARCHAR(255) NOT NULL, payload_digest CHAR(64) NOT NULL, payload_size BIGINT NOT NULL, INDEX jobs_state(state,job_id))`,
	`CREATE TABLE IF NOT EXISTS request_keys (request_key VARCHAR(255) PRIMARY KEY, request_digest CHAR(64) NOT NULL, response_digest CHAR(64) NOT NULL, response_size BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS event_clock (id BIGINT PRIMARY KEY, sequence BIGINT NOT NULL)`,
	`INSERT IGNORE INTO event_clock (id,sequence) VALUES (1,0)`,
	`CREATE TABLE IF NOT EXISTS events (sequence BIGINT PRIMARY KEY, kind VARCHAR(255) NOT NULL, job_id VARCHAR(255) NOT NULL, payload TEXT NOT NULL, created_at DATETIME(6) NOT NULL, INDEX events_job(job_id,sequence))`,
	`CREATE TABLE IF NOT EXISTS daemon_markers (name VARCHAR(255) PRIMARY KEY, payload_digest CHAR(64) NOT NULL, payload_size BIGINT NOT NULL)`,
}

// Migrate applies numbered, replayable forward migrations. Dolt DDL can commit
// implicitly, so all statements are idempotent before recording each version.
// Each version is its own Dolt commit naming it (FAE-CP-008).
func (s *Store) Migrate(ctx context.Context) error { return s.migrateTo(ctx, SchemaVersion) }

func (s *Store) migrateTo(ctx context.Context, target int) error {
	if _, e := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS migrations (version BIGINT PRIMARY KEY, applied_at DATETIME(6) NOT NULL)`); e != nil {
		return e
	}
	var version int
	if e := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM migrations`).Scan(&version); e != nil {
		return e
	}
	if version > SchemaVersion {
		return fmt.Errorf("schema %d is newer than supported %d", version, SchemaVersion)
	}
	migrations := [][]string{SchemaStatements, Migration2, Migration3, Migration4, Migration5, Migration6, Migration7, Migration8, Migration9, Migration10, Migration11, Migration12}
	// Go steps run after a version's statements, in its transaction.
	steps := map[int][]func(context.Context, *sql.Tx) error{8: {migration8Columns, s.backfillRecords}, 9: {migration9Columns, s.backfillFamilies}, 10: {s.migration10}, 11: {migration11Columns, s.backfillBoardColumns}, 12: {migration12Columns, s.backfillTreeIndex}}
	for next := version + 1; next <= target; next++ {
		err := s.transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
			for _, statement := range migrations[next-1] {
				if _, e := tx.ExecContext(ctx, statement); e != nil {
					return e
				}
			}
			for _, step := range steps[next] {
				if e := step(ctx, tx); e != nil {
					return e
				}
			}
			if _, e := tx.ExecContext(ctx, `INSERT INTO migrations (version,applied_at) VALUES (?,UTC_TIMESTAMP(6))`, next); e != nil {
				return e
			}
			return s.versionCommit(ctx, tx, fmt.Sprintf("migration %d by daemon", next))
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// versionCommit makes the transaction's writes one Dolt commit (FAE-CP-008).
func (s *Store) versionCommit(ctx context.Context, tx *sql.Tx, message string) error {
	if !s.options.VersionCommits {
		return nil
	}
	_, e := tx.ExecContext(ctx, `CALL DOLT_COMMIT('-Am', ?)`, message)
	return e
}

// Migration3 serializes each durable resource pool on a single conflict row.
var Migration3 = []string{`CREATE TABLE IF NOT EXISTS resource_pools (pool_key VARCHAR(255) PRIMARY KEY, kind VARCHAR(64) NOT NULL, version BIGINT NOT NULL, payload LONGTEXT NOT NULL)`}

var Migration4 = []string{`CREATE TABLE IF NOT EXISTS provider_observations (observation_id VARCHAR(255) PRIMARY KEY, digest CHAR(64) NOT NULL, payload TEXT NOT NULL)`}

var Migration6 = []string{`CREATE INDEX IF NOT EXISTS events_created ON events (created_at,sequence)`, `CREATE TABLE IF NOT EXISTS event_retention (id BIGINT PRIMARY KEY, floor_sequence BIGINT NOT NULL)`, `INSERT IGNORE INTO event_retention (id,floor_sequence) VALUES (1,0)`}

// TurnCheckpointsTableDDL defines the durable agentic turn checkpoints table (FAE-RT-019).
const TurnCheckpointsTableDDL = `CREATE TABLE IF NOT EXISTS turn_checkpoints (task_id VARCHAR(255) NOT NULL, stage_id VARCHAR(255) NOT NULL, visit BIGINT NOT NULL, attempt BIGINT NOT NULL, provider_session VARCHAR(255) NOT NULL, worktree_tree VARCHAR(255) NOT NULL, assistant_turns BIGINT NOT NULL DEFAULT 0, updated_at DATETIME(6) NOT NULL, PRIMARY KEY(task_id,stage_id,visit,attempt))`

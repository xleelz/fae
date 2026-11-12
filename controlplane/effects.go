package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// RecordEffect writes an immutable content-addressed effect checkpoint and its
// audit/event atomically. Transaction retries never execute the external effect.
func (s *Store) RecordEffect(ctx context.Context, name, task, phase string, payload json.RawMessage) error {
	if name == "" || len(name) > 255 {
		return errors.New("invalid effect marker")
	}
	if phase != "intent" && phase != "started" && phase != "performed" && phase != "observed" {
		return errors.New("invalid effect phase")
	}
	canonical, e := CanonicalJSON(payload)
	if e != nil {
		return e
	}
	descriptor, _ := json.Marshal(map[string]any{"name": name, "task": task, "phase": phase, "digest": Digest(canonical), "size": len(canonical)})
	return s.Mutate(ctx, Audit{Actor: "daemon", Action: "external effect " + phase, Inputs: descriptor, Outcome: "recorded"}, func(ctx context.Context, tx *sql.Tx) error {
		var digest string
		e := tx.QueryRowContext(ctx, "SELECT payload_digest FROM daemon_markers WHERE name=?", name).Scan(&digest)
		if e == nil {
			if digest != Digest(canonical) {
				return errors.New("immutable effect checkpoint differs")
			}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e = s.putBlob(ctx, tx, canonical); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO daemon_markers (name,payload_digest,payload_size) VALUES (?,?,?)", name, Digest(canonical), len(canonical)); e != nil {
			return e
		}
		// The checkpoint is authoritative; external_effects projects the call
		// it records (FAE-EXT-006, FAE-CP-011).
		if row, ok := effectFromCheckpoint(phase, canonical); ok {
			if row.Task == "" {
				row.Task = task
			}
			if e = s.upsertEffectTx(ctx, tx, row, time.Now().UTC()); e != nil {
				return e
			}
		}
		if e = lockEvents(ctx, tx); e != nil {
			return e
		}
		return s.appendEvents(ctx, tx, task, []Event{{Kind: "external-effect-" + phase, JobID: task, Payload: canonical}}, "")
	})
}

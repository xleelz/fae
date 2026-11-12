package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// WorkflowSlotWaitAlert records one alert for a durable waiting episode, even
// when the daemon restarts after the threshold or an acknowledgement is lost.
func (s *Store) WorkflowSlotWaitAlert(ctx context.Context, episode string, raw json.RawMessage) (bool, error) {
	raised := false
	err := s.Mutate(ctx, Audit{Actor: "daemon", Action: "workflow-slot-wait-alert", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		key := "workflow-slot-alert/" + Digest([]byte(episode))
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT name FROM daemon_markers WHERE name=?`, key).Scan(&existing)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err = lockEvents(ctx, tx); err != nil {
			return err
		}
		if err = s.putBlob(ctx, tx, raw); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO daemon_markers(name,payload_digest,payload_size) VALUES(?,?,?)`, key, Digest(raw), len(raw)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO alerts(severity,source,text_digest,created_at) VALUES(?,?,?,?)`, "warning", "workflow-slots", Digest(raw), time.Now().UTC()); err != nil {
			return err
		}
		if err = s.appendEvents(ctx, tx, "", []Event{{Kind: "workflow-slot-wait-alert", Payload: raw}}, ""); err != nil {
			return err
		}
		raised = true
		return nil
	})
	return raised, err
}

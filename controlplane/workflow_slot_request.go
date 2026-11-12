package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
)

// SetWorkflowSlotRequest commits the instance request, attribution audit and
// change event together. The caller holds the host ledger transaction first.
func (s *Store) SetWorkflowSlotRequest(ctx context.Context, a Audit, raw json.RawMessage) error {
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockEvents(ctx, tx); err != nil {
			return err
		}
		if err := s.putBlob(ctx, tx, raw); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `REPLACE INTO daemon_markers(name,payload_digest,payload_size) VALUES(?,?,?)`, "workflow-slot-request", Digest(raw), len(raw)); err != nil {
			return err
		}
		return s.appendEvents(ctx, tx, "", []Event{{Kind: "workflow-limit-set", Payload: raw}}, "")
	})
}

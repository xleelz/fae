package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// RecordIdleAction commits the operator line and event in one transaction. An
// external retirement is performed outside the transaction and never replayed
// by SQL retries.
func (s *Store) RecordIdleAction(ctx context.Context, kind string, payload json.RawMessage) error {
	switch kind {
	case "idle-session-shed", "idle-session-retired", "idle-session-reconciled", "idle-maintenance-refused", "PaneCapStarvation":
	default:
		return errors.New("unsupported idle action")
	}
	return s.Mutate(ctx, Audit{Actor: "daemon", Action: kind, Inputs: payload, Outcome: string(payload)}, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockEvents(ctx, tx); err != nil {
			return err
		}
		return s.appendEvents(ctx, tx, "", []Event{{Kind: kind, Payload: payload}}, "")
	})
}

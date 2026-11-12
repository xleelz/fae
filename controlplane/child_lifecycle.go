package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// RecordChildLifecycle commits one audited supervised-child lifecycle event
// (FAE-API-020: started, crashed, restarted). The payload names the child and
// must carry no secrets.
func (s *Store) RecordChildLifecycle(ctx context.Context, phase string, payload json.RawMessage) error {
	switch phase {
	case "started", "crashed", "restarted":
	default:
		return errors.New("unsupported child lifecycle phase")
	}
	kind := "child-" + phase
	return s.Mutate(ctx, Audit{Actor: "daemon", Action: kind, Inputs: payload, Outcome: phase}, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockEvents(ctx, tx); err != nil {
			return err
		}
		return s.appendEvents(ctx, tx, "", []Event{{Kind: kind, Payload: payload}}, "")
	})
}

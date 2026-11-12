package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// LifecycleKinds are FAE-API-020's daemon and Dolt-child lifecycle events.
var LifecycleKinds = map[string]bool{
	"daemon-starting": true, "daemon-ready": true, "daemon-shutting-down": true, "daemon-stopped": true,
	"dolt-started": true, "dolt-crashed": true, "dolt-restarted": true,
}

// RecordLifecycle commits one audited instance lifecycle event. The payload
// names the process and must carry no secrets.
func (s *Store) RecordLifecycle(ctx context.Context, kind string, payload json.RawMessage) error {
	if !LifecycleKinds[kind] {
		return errors.New("unsupported lifecycle event " + kind)
	}
	return s.Mutate(ctx, Audit{Actor: "daemon", Action: kind, Inputs: payload, Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockEvents(ctx, tx); err != nil {
			return err
		}
		return s.appendEvents(ctx, tx, "", []Event{{Kind: kind, Payload: payload}}, "")
	})
}

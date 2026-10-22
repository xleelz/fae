package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// ControllerAction atomically records the lifecycle marker, operator audit,
// event, and (for failures) alert. Supervisors never write the plane themselves.
func (s *Store) ControllerAction(ctx context.Context, a Audit, marker json.RawMessage, alert bool) error {
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if e := lockEvents(ctx, tx); e != nil {
			return e
		}
		if e := s.putBlob(ctx, tx, marker); e != nil {
			return e
		}
		if strings.HasPrefix(a.Action, "controller-maintenance-") {
			if _, e := tx.ExecContext(ctx, `REPLACE INTO daemon_markers(name,payload_digest,payload_size) VALUES(?,?,?)`, "controller-maintenance", Digest(marker), len(marker)); e != nil {
				return e
			}
		}
		if alert {
			if _, e := tx.ExecContext(ctx, `INSERT INTO alerts(severity,source,text_digest,created_at) VALUES(?,?,?,?)`, "error", "controller", Digest(marker), time.Now().UTC()); e != nil {
				return e
			}
		}
		return s.appendEvents(ctx, tx, "", []Event{{Kind: a.Action, Payload: marker}}, "")
	})
}

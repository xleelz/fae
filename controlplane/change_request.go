package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
)

func (s *Store) RecordChangeRequestDisposition(ctx context.Context, a Audit, target, key string, body json.RawMessage) error {
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockEvents(ctx, tx); err != nil {
			return err
		}
		if err := s.putMarkerTx(ctx, tx, key, body); err != nil {
			return err
		}
		return s.appendEvents(ctx, tx, target, []Event{{Kind: "change-request-disposition", Payload: body}}, "change-request-disposition")
	})
}

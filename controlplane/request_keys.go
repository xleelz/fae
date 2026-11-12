package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// LookupRequest returns the recorded response of a keyed mutating request
// (FAE-API-012); the same key with a different request is a conflict.
func (s *Store) LookupRequest(ctx context.Context, key string, request []byte) (json.RawMessage, bool, error) {
	if key == "" {
		return nil, false, nil
	}
	var recorded string
	var ref BlobRef
	e := s.db.QueryRowContext(ctx, `SELECT request_digest,response_digest,response_size FROM request_keys WHERE request_key=?`, key).Scan(&recorded, &ref.Digest, &ref.Size)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, false, nil
	}
	if e != nil {
		return nil, false, e
	}
	if recorded != Digest(request) {
		return nil, false, ErrIdempotencyConflict
	}
	raw, e := s.GetBlob(ctx, ref)
	return raw, e == nil, e
}

// RecordRequest records a keyed request's response once: a second record of
// the same request is a no-op and of another request a conflict.
func (s *Store) RecordRequest(ctx context.Context, key string, request, response []byte) error {
	if key == "" || len(key) > 255 {
		return errors.New("request key must contain 1..255 bytes")
	}
	return s.transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var recorded string
		e := tx.QueryRowContext(ctx, `SELECT request_digest FROM request_keys WHERE request_key=?`, key).Scan(&recorded)
		if e == nil {
			if recorded != Digest(request) {
				return ErrIdempotencyConflict
			}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e = s.putBlob(ctx, tx, response); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO request_keys (request_key,request_digest,response_digest,response_size) VALUES (?,?,?,?)`, key, Digest(request), Digest(response), len(response))
		return e
	})
}

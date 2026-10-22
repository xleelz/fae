package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrTargetRetired = errors.New("target retired")

type TargetRevision struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Hash     string `json:"hash"`
	Retired  bool   `json:"retired"`
}

func (s *Store) WriteTarget(ctx context.Context, a Audit, id string, raw json.RawMessage, validate func([]byte) error) (r TargetRevision, err error) {
	if id == "" || len(id) > 255 || validate == nil {
		return r, errors.New("target identity and validator required")
	}
	hash, canonical, err := ContentHash(raw)
	if err != nil {
		return r, err
	}
	if err = validate(canonical); err != nil {
		return r, err
	}
	err = s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		adapter, adapterErr := targetAdapterID(canonical)
		if adapterErr != nil {
			return adapterErr
		}
		if adapter != "" {
			if e := lockEvents(ctx, tx); e != nil {
				return e
			}
			if e := adapterActiveTx(ctx, tx, adapter); e != nil {
				return e
			}
		}
		var retired int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM daemon_markers WHERE name=?`, targetRetiredKey(id)).Scan(&retired); e != nil {
			return e
		}
		if retired > 0 {
			return ErrTargetRetired
		}
		var latest int64
		if e := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM target_configurations WHERE id=?`, id).Scan(&latest); e != nil {
			return e
		}
		if e := s.putBlob(ctx, tx, canonical); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, `INSERT INTO target_configurations (id,revision,content_digest,created_at) VALUES (?,?,?,?)`, id, latest+1, hash, time.Now().UTC())
		r = TargetRevision{ID: id, Revision: latest + 1, Hash: hash}
		return e
	})
	return
}
func (s *Store) GetTarget(ctx context.Context, id string, revision int64) (r TargetRevision, raw []byte, err error) {
	if id == "" || revision < 0 {
		return r, nil, errors.New("target identity and nonnegative revision required")
	}
	q := `SELECT id,revision,content_digest FROM target_configurations WHERE id=?`
	args := []any{id}
	if revision > 0 {
		q += ` AND revision=?`
		args = append(args, revision)
	}
	q += ` ORDER BY revision DESC LIMIT 1`
	if err = s.db.QueryRowContext(ctx, q, args...).Scan(&r.ID, &r.Revision, &r.Hash); err != nil {
		return
	}
	var size int64
	if err = s.db.QueryRowContext(ctx, `SELECT size FROM blobs WHERE digest=?`, r.Hash).Scan(&size); err != nil {
		return
	}
	raw, err = s.GetBlob(ctx, BlobRef{Digest: r.Hash, Size: size})
	if err == nil {
		if e := s.TargetActive(ctx, id); e != nil {
			if errors.Is(e, ErrTargetRetired) {
				r.Retired = true
			} else {
				err = e
			}
		}
	}
	return
}
func (s *Store) ListTargets(ctx context.Context) (result []TargetRevision, err error) {
	result = []TargetRevision{}
	rows, err := s.db.QueryContext(ctx, `SELECT id,revision,content_digest FROM target_configurations ORDER BY id,revision LIMIT 10001`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var r TargetRevision
		if err = rows.Scan(&r.ID, &r.Revision, &r.Hash); err != nil {
			return
		}

		result = append(result, r)
		if len(result) > 10000 {
			return nil, errors.New("target listing bound exceeded")
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range result {
		if e := s.TargetActive(ctx, result[i].ID); e != nil {
			if !errors.Is(e, ErrTargetRetired) {
				return nil, e
			}
			result[i].Retired = true
		}
	}
	return result, nil
}

func targetRetiredKey(id string) string { return "target-retired/" + Digest([]byte(id)) }
func (s *Store) TargetActive(ctx context.Context, id string) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM daemon_markers WHERE name=?`, targetRetiredKey(id)).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return ErrTargetRetired
	}
	return nil
}
func (s *Store) RetireTarget(ctx context.Context, a Audit, id, reason string) error {
	if reason == "" {
		return errors.New("target retirement reason required")
	}
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM target_configurations WHERE id=?`, id).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return sql.ErrNoRows
		}
		key := targetRetiredKey(id)
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM daemon_markers WHERE name=?`, key).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return nil
		}
		raw, _ := json.Marshal(map[string]any{"id": id, "reason": reason, "actor": a.Actor, "retired_at": time.Now().UTC()})
		_, canonical, err := ContentHash(raw)
		if err != nil {
			return err
		}
		if err = s.putBlob(ctx, tx, canonical); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO daemon_markers (name,payload_digest,payload_size) VALUES (?,?,?)`, key, Digest(canonical), len(canonical))
		return err
	})
}

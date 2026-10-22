package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrAdapterRetired = errors.New("adapter retired")

func adapterRetiredKey(id string) string { return "adapter-retired/" + Digest([]byte(id)) }

func adapterActiveTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) error {
	if !adapterID(id) {
		return errors.New("adapter identity invalid")
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM daemon_markers WHERE name=?`, adapterRetiredKey(id)).Scan(&n); err != nil {
		return err
	}
	if !AdapterAcceptsBinding(n > 0) {
		return ErrAdapterRetired
	}
	return nil
}
func (s *Store) AdapterActive(ctx context.Context, id string) error {
	return adapterActiveTx(ctx, s.db, id)
}
func (s *Store) RetireAdapter(ctx context.Context, a Audit, id, reason string) error {
	if !adapterID(id) || strings.TrimSpace(reason) == "" || len(reason) > 4096 {
		return errors.New("adapter id and bounded retirement reason required")
	}
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockEvents(ctx, tx); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM daemon_markers WHERE name=?`, adapterPrefix(id)+"head").Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return sql.ErrNoRows
		}
		err := adapterActiveTx(ctx, tx, id)
		if errors.Is(err, ErrAdapterRetired) {
			return nil
		}
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(map[string]any{"id": id, "reason": reason, "actor": a.Actor, "retired_at": time.Now().UTC()})
		if err = s.putBlob(ctx, tx, raw); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO daemon_markers(name,payload_digest,payload_size) VALUES (?,?,?)`, adapterRetiredKey(id), Digest(raw), len(raw))
		return err
	})
}

// PutActiveAdapterMarker linearizes a new preparation intent against retirement.
func (s *Store) PutActiveAdapterMarker(ctx context.Context, a Audit, key string, raw json.RawMessage, id string) error {
	_, raw, err := ContentHash(raw)
	if err != nil {
		return err
	}
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockEvents(ctx, tx); err != nil {
			return err
		}
		if err := adapterActiveTx(ctx, tx, id); err != nil {
			return err
		}
		if err := s.putBlob(ctx, tx, raw); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO daemon_markers(name,payload_digest,payload_size) VALUES (?,?,?)`, key, Digest(raw), len(raw))
		return err
	})
}
func targetAdapterID(raw []byte) (string, error) {
	var d struct {
		Normal *struct {
			Adapter *struct {
				ID string `json:"id"`
			} `json:"adapter"`
		} `json:"normal"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", err
	}
	if d.Normal != nil && d.Normal.Adapter != nil {
		return d.Normal.Adapter.ID, nil
	}
	return "", nil
}
func checkTargetAdapterTx(ctx context.Context, tx *sql.Tx, raw []byte) error {
	id, err := targetAdapterID(raw)
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	return adapterActiveTx(ctx, tx, id)
}
func checkJobAdapterTx(ctx context.Context, tx *sql.Tx, j JobSnapshot) error {
	var p struct {
		Admission struct {
			TargetConfig string `json:"target_config"`
		} `json:"admission"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	if p.Admission.TargetConfig == "" {
		return nil
	}
	var hash string
	if err := tx.QueryRowContext(ctx, `SELECT content_digest FROM target_configurations WHERE id=? AND revision=?`, j.TargetID, j.TargetRevision).Scan(&hash); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT chunk_index,data FROM blob_chunks WHERE digest=? ORDER BY chunk_index`, hash)
	if err != nil {
		return err
	}
	var raw []byte
	n := 0
	for rows.Next() {
		var i int
		var b []byte
		if err = rows.Scan(&i, &b); err != nil {
			rows.Close()
			return err
		}
		if i != n {
			rows.Close()
			return errors.New("target blob chunk missing")
		}
		n++
		raw = append(raw, b...)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if Digest(raw) != hash {
		return errors.New("target blob integrity failure")
	}
	return checkTargetAdapterTx(ctx, tx, raw)
}

func (s *Store) AdapterRetirement(ctx context.Context, id string) (json.RawMessage, error) {
	if !adapterID(id) {
		return nil, errors.New("adapter identity invalid")
	}
	return s.GetMarker(ctx, adapterRetiredKey(id))
}

package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// AdapterRevision points to an immutable, integrity-checked installed document.
type AdapterRevision struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Hash     string `json:"hash"`
	Retired  bool   `json:"retired,omitempty"`
}
type adapterStored struct {
	AdapterRevision
	Document json.RawMessage `json:"document"`
}

func adapterPrefix(id string) string { return "adapter/" + Digest([]byte(id)) + "/" }
func adapterID(id string) bool {
	return strings.TrimSpace(id) == id && id != "" && len(id) <= 255 && !strings.ContainsAny(id, "\x00\r\n")
}
func (s *Store) WriteAdapter(ctx context.Context, a Audit, id string, raw json.RawMessage) (r AdapterRevision, err error) {
	if !adapterID(id) || len(raw) > 1<<20 {
		return r, errors.New("adapter id/document bound invalid")
	}
	hash, canonical, err := ContentHash(raw)
	if err != nil {
		return r, err
	}
	prefix := adapterPrefix(id)
	err = s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockEvents(ctx, tx); err != nil {
			return err
		}
		if err := adapterActiveTx(ctx, tx, id); err != nil {
			return err
		}
		var head BlobRef
		var n int64
		e := tx.QueryRowContext(ctx, `SELECT payload_digest,payload_size FROM daemon_markers WHERE name=? FOR UPDATE`, prefix+"head").Scan(&head.Digest, &head.Size)
		if e == nil {
			var b []byte
			if e = tx.QueryRowContext(ctx, `SELECT data FROM blob_chunks WHERE digest=? AND chunk_index=0`, head.Digest).Scan(&b); e != nil {
				return e
			}
			if int64(len(b)) != head.Size || Digest(b) != head.Digest {
				return errors.New("adapter head integrity failure")
			}
			if e = json.Unmarshal(b, &n); e != nil {
				return e
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if n < 0 || n >= 1000 {
			return errors.New("adapter revision bound exceeded")
		}
		r = AdapterRevision{ID: id, Revision: n + 1, Hash: hash}
		b, _ := json.Marshal(adapterStored{AdapterRevision: r, Document: canonical})
		if e = s.putBlob(ctx, tx, b); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO daemon_markers(name,payload_digest,payload_size) VALUES (?,?,?)`, fmt.Sprintf("%s%020d", prefix, r.Revision), Digest(b), len(b)); e != nil {
			return e
		}
		b, _ = json.Marshal(r.Revision)
		if e = s.putBlob(ctx, tx, b); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `REPLACE INTO daemon_markers(name,payload_digest,payload_size) VALUES (?,?,?)`, prefix+"head", Digest(b), len(b))
		return e
	})
	return
}
func (s *Store) GetAdapter(ctx context.Context, id string, revision int64) (AdapterRevision, json.RawMessage, error) {
	var zero AdapterRevision
	if !adapterID(id) || revision < 0 {
		return zero, nil, errors.New("adapter selector invalid")
	}
	if revision == 0 {
		b, e := s.GetMarker(ctx, adapterPrefix(id)+"head")
		if e != nil {
			return zero, nil, e
		}
		if e = json.Unmarshal(b, &revision); e != nil {
			return zero, nil, e
		}
	}
	b, e := s.GetMarker(ctx, fmt.Sprintf("%s%020d", adapterPrefix(id), revision))
	if e != nil {
		return zero, nil, e
	}
	var stored adapterStored
	if e = json.Unmarshal(b, &stored); e != nil {
		return zero, nil, e
	}
	h, _, e := ContentHash(stored.Document)
	if e != nil || stored.ID != id || stored.Revision != revision || h != stored.Hash {
		return zero, nil, errors.New("adapter identity/hash mismatch")
	}
	if e = s.AdapterActive(ctx, id); errors.Is(e, ErrAdapterRetired) {
		stored.Retired = true
	} else if e != nil {
		return zero, nil, e
	}
	return stored.AdapterRevision, stored.Document, nil
}
func (s *Store) ListAdapters(ctx context.Context) ([]AdapterRevision, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT payload_digest,payload_size FROM daemon_markers WHERE name LIKE 'adapter/%' AND name NOT LIKE '%/head' ORDER BY name LIMIT 10001`)
	if e != nil {
		return nil, e
	}
	refs := []BlobRef{}
	for rows.Next() {
		var ref BlobRef
		if e = rows.Scan(&ref.Digest, &ref.Size); e != nil {
			rows.Close()
			return nil, e
		}
		refs = append(refs, ref)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if len(refs) > 10000 {
		return nil, errors.New("adapter listing bound exceeded")
	}
	out := []AdapterRevision{}
	for _, ref := range refs {
		b, e := s.GetBlob(ctx, ref)
		if e != nil {
			return nil, e
		}
		var v adapterStored
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		h, _, e := ContentHash(v.Document)
		if e != nil || h != v.Hash {
			return nil, errors.New("adapter document integrity failure")
		}
		if e = s.AdapterActive(ctx, v.ID); errors.Is(e, ErrAdapterRetired) {
			v.Retired = true
		} else if e != nil {
			return nil, e
		}
		out = append(out, v.AdapterRevision)
	}
	return out, nil
}

package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ProphetEntry is an immutable chat page entry; reply entries cite the request.
type ProphetEntry struct {
	Sequence int64           `json:"sequence"`
	ID       string          `json:"id"`
	Task     string          `json:"task,omitempty"`
	Author   string          `json:"author"`
	Text     string          `json:"text"`
	ReplyTo  string          `json:"reply_to,omitempty"`
	Time     time.Time       `json:"time"`
	Detail   json.RawMessage `json:"detail,omitempty"`
}

func prophetPrefix(task string) string { return "prophet/" + Digest([]byte(task)) + "/" }
func (s *Store) AppendProphet(ctx context.Context, a Audit, entry ProphetEntry) (ProphetEntry, error) {
	if entry.ID == "" || len(entry.ID) > 255 || len(entry.Task) > 255 || len(entry.Text) > 1<<20 || (entry.Author != "operator" && entry.Author != "prophet" && entry.Author != "system") {
		return entry, errors.New("invalid Prophet entry")
	}
	prefix := prophetPrefix(entry.Task)
	err := s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		var n int64
		// The daemon serializes appenders; the SQL index is immutable and commits with its counter.
		var ref BlobRef
		err := tx.QueryRowContext(ctx, `SELECT payload_digest,payload_size FROM daemon_markers WHERE name=? FOR UPDATE`, prefix+"head").Scan(&ref.Digest, &ref.Size)
		if err == nil {
			var raw []byte
			if err = tx.QueryRowContext(ctx, `SELECT data FROM blob_chunks WHERE digest=? AND chunk_index=0`, ref.Digest).Scan(&raw); err != nil {
				return err
			}
			if err = json.Unmarshal(raw, &n); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		entry.Sequence = n + 1
		entry.Time = time.Now().UTC()
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if err = s.putBlob(ctx, tx, raw); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO daemon_markers(name,payload_digest,payload_size) VALUES (?,?,?)`, fmt.Sprintf("%s%020d", prefix, entry.Sequence), Digest(raw), len(raw)); err != nil {
			return err
		}
		head, _ := json.Marshal(entry.Sequence)
		if err = s.putBlob(ctx, tx, head); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `REPLACE INTO daemon_markers(name,payload_digest,payload_size) VALUES (?,?,?)`, prefix+"head", Digest(head), len(head))
		return err
	})
	return entry, err
}
func (s *Store) ProphetPage(ctx context.Context, task string, after int64, limit int) ([]ProphetEntry, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return nil, errors.New("Prophet page requires after >= 0 and limit 1..100")
	}
	prefix := prophetPrefix(task)
	rows, err := s.db.QueryContext(ctx, `SELECT payload_digest,payload_size FROM daemon_markers WHERE name>? AND name<? ORDER BY name LIMIT ?`, fmt.Sprintf("%s%020d", prefix, after), prefix+"head", limit)
	if err != nil {
		return nil, err
	}
	var refs []BlobRef
	for rows.Next() {
		var r BlobRef
		if err = rows.Scan(&r.Digest, &r.Size); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []ProphetEntry{}
	for _, r := range refs {
		raw, e := s.GetBlob(ctx, r)
		if e != nil {
			return nil, e
		}
		var v ProphetEntry
		if e = json.Unmarshal(raw, &v); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) ProphetHead(ctx context.Context, task string) (int64, error) {
	raw, e := s.GetMarker(ctx, prophetPrefix(task)+"head")
	if errors.Is(e, sql.ErrNoRows) {
		return 0, nil
	}
	if e != nil {
		return 0, e
	}
	var n int64
	e = json.Unmarshal(raw, &n)
	return n, e
}

// MessageText resolves only an already-scoped citation; callers must never enumerate other tasks' messages.
func (s *Store) MessageText(ctx context.Context, id string) (string, error) {
	var r BlobRef
	if e := s.db.QueryRowContext(ctx, `SELECT m.text_digest,b.size FROM operator_messages m JOIN blobs b ON b.digest=m.text_digest WHERE m.message_id=?`, id).Scan(&r.Digest, &r.Size); e != nil {
		return "", e
	}
	raw, e := s.GetBlob(ctx, r)
	return string(raw), e
}

// ProphetGlobalMetadata returns bounded authoritative molecule and alert summaries.
func (s *Store) ProphetGlobalMetadata(ctx context.Context) (map[string]any, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT molecule_id,workflow_id,revision FROM molecules ORDER BY created_at DESC,molecule_id LIMIT 101`)
	if e != nil {
		return nil, e
	}
	molecules := []map[string]any{}
	for rows.Next() {
		var id, w string
		var rev int64
		if e = rows.Scan(&id, &w, &rev); e != nil {
			rows.Close()
			return nil, e
		}
		molecules = append(molecules, map[string]any{"id": id, "workflow": w, "revision": rev})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	moreMolecules := len(molecules) > 100
	if moreMolecules {
		molecules = molecules[:100]
	}
	rows, e = s.db.QueryContext(ctx, `SELECT a.id,a.severity,a.source,a.created_at,a.text_digest,b.size FROM alerts a JOIN blobs b ON b.digest=a.text_digest ORDER BY a.id DESC LIMIT 21`)
	if e != nil {
		return nil, e
	}
	type alert struct {
		ID               int64
		Severity, Source string
		Time             time.Time
		Ref              BlobRef
	}
	entries := []alert{}
	for rows.Next() {
		var a alert
		if e = rows.Scan(&a.ID, &a.Severity, &a.Source, &a.Time, &a.Ref.Digest, &a.Ref.Size); e != nil {
			rows.Close()
			return nil, e
		}
		entries = append(entries, a)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	moreAlerts := len(entries) > 20
	if moreAlerts {
		entries = entries[:20]
	}
	alerts := []map[string]any{}
	for _, a := range entries {
		v := map[string]any{"id": a.ID, "severity": a.Severity, "source": a.Source, "time": a.Time, "text_digest": a.Ref.Digest, "text_bytes": a.Ref.Size}
		if a.Ref.Size <= 16384 {
			raw, e := s.GetBlob(ctx, a.Ref)
			if e != nil {
				return nil, e
			}
			v["text"] = string(raw)
		} else {
			v["text_omitted"] = true
		}
		alerts = append(alerts, v)
	}
	return map[string]any{"molecules": molecules, "more_molecules": moreMolecules, "alerts": alerts, "more_alerts": moreAlerts}, nil
}

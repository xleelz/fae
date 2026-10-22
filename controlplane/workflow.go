package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type Revision struct {
	WorkflowID string
	Number     int64
	Parent     int64
	Name       string
	Hash       string
}

// WriteWorkflow validates before opening a transaction and assigns the next immutable revision.
func (s *Store) WriteWorkflow(ctx context.Context, a Audit, id, name string, content json.RawMessage, validate func([]byte) error) (Revision, error) {
	var r Revision
	if id == "" || name == "" || validate == nil {
		return r, errors.New("workflow identity and validator required")
	}
	hash, canonical, e := ContentHash(content)
	if e != nil {
		return r, e
	}
	if e = validate(canonical); e != nil {
		return r, e
	}
	e = s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		var retired int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_status WHERE workflow_id=? AND retired_at IS NOT NULL`, id).Scan(&retired); e != nil {
			return e
		}
		if retired > 0 {
			return errors.New("workflow retired")
		}
		var latest int64
		if e := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM workflows WHERE workflow_id=?`, id).Scan(&latest); e != nil {
			return e
		}
		if e := s.putBlob(ctx, tx, canonical); e != nil {
			return e
		}
		var parent any
		if latest > 0 {
			parent = latest
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO workflows (workflow_id,revision,parent_revision,name,content_digest,created_by,created_at) VALUES (?,?,?,?,?,?,?)`, id, latest+1, parent, name, hash, a.Actor, time.Now().UTC()); e != nil {
			return e
		}
		r = Revision{id, latest + 1, latest, name, hash}
		return nil
	})
	return r, e
}

// BindMolecule retains a single immutable identity for a workflow/revision pair.
func (s *Store) BindMolecule(ctx context.Context, a Audit, id string, revision int64) (string, error) {
	var molecule string
	e := s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		var retired int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_status WHERE workflow_id=? AND retired_at IS NOT NULL`, id).Scan(&retired); e != nil {
			return e
		}
		if retired > 0 {
			return errors.New("workflow retired")
		}
		var hash string
		if e := tx.QueryRowContext(ctx, `SELECT content_digest FROM workflows WHERE workflow_id=? AND revision=?`, id, revision).Scan(&hash); e != nil {
			return e
		}
		var existing string
		e := tx.QueryRowContext(ctx, `SELECT content_digest FROM molecules WHERE workflow_id=? AND revision=?`, id, revision).Scan(&existing)
		if e == nil {
			if existing != hash {
				return errors.New("molecule content hash mismatch")
			}
			molecule = hash
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO molecules (molecule_id,workflow_id,revision,content_digest,created_at) VALUES (?,?,?,?,?)`, hash, id, revision, hash, time.Now().UTC())
		molecule = hash
		return e
	})
	return molecule, e
}
func (s *Store) RetireWorkflow(ctx context.Context, a Audit, id, reason string) error {
	if reason == "" {
		return errors.New("retirement reason required")
	}
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		var count int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflows WHERE workflow_id=?`, id).Scan(&count); e != nil {
			return e
		}
		if count == 0 {
			return sql.ErrNoRows
		}
		_, e := tx.ExecContext(ctx, `INSERT IGNORE INTO workflow_status (workflow_id,retired_at,retired_by,reason) VALUES (?,?,?,?)`, id, time.Now().UTC(), a.Actor, reason)
		return e
	})
}

// RecordArtifact stores canonical content and immutable provenance in the same transaction.
func (s *Store) RecordArtifact(ctx context.Context, a Audit, taskID, kind, schemaID string, version int, content, provenance json.RawMessage, validate func([]byte) error) (string, error) {
	if taskID == "" || kind == "" || schemaID == "" || version < 1 || validate == nil {
		return "", errors.New("artifact metadata and validator required")
	}
	hash, b, e := ContentHash(content)
	if e != nil {
		return "", e
	}
	if e = validate(b); e != nil {
		return "", e
	}
	p, e := CanonicalJSON(provenance)
	if e != nil {
		return "", e
	}
	if len(p) > s.options.InlineLimit {
		return "", errors.New("provenance exceeds inline limit")
	}
	e = s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if e := s.putBlob(ctx, tx, b); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, `INSERT INTO artifacts (artifact_id,task_id,kind,schema_id,schema_version,content_digest,provenance,created_at) VALUES (?,?,?,?,?,?,?,?)`, hash, taskID, kind, schemaID, version, hash, string(p), time.Now().UTC())
		return e
	})
	return hash, e
}

// GetWorkflow resolves a specific immutable revision, or latest when revision is zero.
func (s *Store) GetWorkflow(ctx context.Context, id string, revision int64) (Revision, []byte, error) {
	var r Revision
	if id == "" || revision < 0 {
		return r, nil, errors.New("workflow identity and nonnegative revision required")
	}
	query := `SELECT workflow_id,revision,COALESCE(parent_revision,0),name,content_digest FROM workflows WHERE workflow_id=?`
	args := []any{id}
	if revision > 0 {
		query += ` AND revision=?`
		args = append(args, revision)
	}
	query += ` ORDER BY revision DESC LIMIT 1`
	if e := s.db.QueryRowContext(ctx, query, args...).Scan(&r.WorkflowID, &r.Number, &r.Parent, &r.Name, &r.Hash); e != nil {
		return r, nil, e
	}
	var size int64
	if e := s.db.QueryRowContext(ctx, `SELECT size FROM blobs WHERE digest=?`, r.Hash).Scan(&size); e != nil {
		return r, nil, e
	}
	content, e := s.GetBlob(ctx, BlobRef{Digest: r.Hash, Size: size})
	return r, content, e
}

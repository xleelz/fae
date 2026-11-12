package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// ErrWorkflowRetired refuses new revisions and new bindings; bound tasks continue.
var ErrWorkflowRetired = errors.New("workflow retired")

type Revision struct {
	WorkflowID string
	Number     int64
	Parent     int64
	Name       string
	Hash       string
}

// WriteWorkflow validates before opening a transaction and assigns the next immutable revision.
func (s *Store) WriteWorkflow(ctx context.Context, a Audit, id, name string, content json.RawMessage, validate func([]byte) error, footprint ...json.RawMessage) (Revision, error) {
	var r Revision
	if len(footprint) > 1 || (len(footprint) == 1 && (!json.Valid(footprint[0]) || len(footprint[0]) > 4096)) {
		return r, errors.New("one bounded JSON workflow footprint required")
	}
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
			return ErrWorkflowRetired
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
		if len(footprint) == 1 {
			if err := s.putMarkerTx(ctx, tx, WorkflowFootprintKey(id, latest+1), footprint[0]); err != nil {
				return err
			}
		}
		r = Revision{id, latest + 1, latest, name, hash}
		return nil
	})
	return r, e
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

type WorkflowRetirement struct {
	WorkflowID string    `json:"workflow_id"`
	RetiredAt  time.Time `json:"retired_at"`
	RetiredBy  string    `json:"retired_by"`
	Reason     string    `json:"reason"`
}

// WorkflowRetirement returns the first retirement record, or nil while active.
func (s *Store) WorkflowRetirement(ctx context.Context, id string) (*WorkflowRetirement, error) {
	r := WorkflowRetirement{WorkflowID: id}
	var at sql.NullTime
	var by, reason sql.NullString
	e := s.db.QueryRowContext(ctx, `SELECT retired_at,retired_by,reason FROM workflow_status WHERE workflow_id=?`, id).Scan(&at, &by, &reason)
	if errors.Is(e, sql.ErrNoRows) || e == nil && !at.Valid {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	r.RetiredAt, r.RetiredBy, r.Reason = at.Time.UTC(), by.String, reason.String
	return &r, nil
}

// WorkflowActive is checked by every admission path before a new binding.
func (s *Store) WorkflowActive(ctx context.Context, id string) error {
	r, e := s.WorkflowRetirement(ctx, id)
	if e != nil {
		return e
	}
	if r != nil {
		return ErrWorkflowRetired
	}
	return nil
}

type WorkflowListing struct {
	WorkflowID string    `json:"workflow_id"`
	Revision   int64     `json:"revision"`
	Parent     int64     `json:"parent_revision"`
	Name       string    `json:"name"`
	Hash       string    `json:"hash"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	Retired    bool      `json:"retired"`
}

// ListWorkflows lists every revision of one workflow, or of all workflows when id is empty.
func (s *Store) ListWorkflows(ctx context.Context, id string) ([]WorkflowListing, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT w.workflow_id,w.revision,COALESCE(w.parent_revision,0),w.name,w.content_digest,w.created_by,w.created_at,st.retired_at FROM workflows w LEFT JOIN workflow_status st ON st.workflow_id=w.workflow_id WHERE (?='' OR w.workflow_id=?) ORDER BY w.workflow_id,w.revision LIMIT 10001`, id, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []WorkflowListing{}
	for rows.Next() {
		var r WorkflowListing
		var retired sql.NullTime
		if e = rows.Scan(&r.WorkflowID, &r.Revision, &r.Parent, &r.Name, &r.Hash, &r.CreatedBy, &r.CreatedAt, &retired); e != nil {
			return nil, e
		}
		r.Retired = retired.Valid
		if out = append(out, r); len(out) > 10000 {
			return nil, errors.New("workflow listing bound exceeded")
		}
	}
	return out, rows.Err()
}

// RecordArtifact stores canonical content once per task, content-addressed,
// with an append-only provenance row naming the recording actor; the caller's
// provenance document is kept verbatim on the artifact row.
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
		var molecule string
		if e := tx.QueryRowContext(ctx, `SELECT molecule_id FROM tasks WHERE task_id=?`, taskID).Scan(&molecule); e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		now := time.Now().UTC()
		var present int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE artifact_id=? AND task_id=?`, hash, taskID).Scan(&present); e != nil {
			return e
		}
		if present == 0 {
			if _, e := tx.ExecContext(ctx, `INSERT INTO artifacts (artifact_id,task_id,kind,schema_id,schema_version,content_digest,provenance,created_at) VALUES (?,?,?,?,?,?,?,?)`, hash, taskID, kind, schemaID, version, hash, string(p), now); e != nil {
				return e
			}
		}
		row := ProvenanceRecord{ArtifactID: hash, TaskID: taskID, MoleculeID: molecule, Kind: kind, DispatchID: a.Action, Stage: kind, Primitive: "Recorded", Role: a.Actor, ProducedAt: now}
		return s.recordArtifactTx(ctx, tx, row, schemaID, int64(version), b, now)
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

func WorkflowFootprintKey(id string, revision int64) string {
	return "workflow-footprint/" + Digest([]byte(id)) + "/" + strconv.FormatInt(revision, 10)
}

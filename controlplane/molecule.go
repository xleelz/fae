package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrMoleculeMismatch refuses a stored molecule that a re-resolution of its
// workflow revision does not reproduce (FAE-WF-006).
var ErrMoleculeMismatch = errors.New("molecule content hash mismatch")

// ErrUnknownRevision refuses a binding to a workflow revision the plane lacks.
var ErrUnknownRevision = errors.New("unknown workflow revision")

// ErrTopologyBound refuses a topology page with more tasks than its bound.
var ErrTopologyBound = errors.New("topology task bound exceeded; narrow the page")

// moleculeResolution is a molecule's resolved content: the workflow identity,
// the revision number and the revision's content hash. Its canonical SHA-256 is
// the molecule id, so two revisions with identical content stay distinct.
func moleculeResolution(workflowID string, revision int64, contentDigest string) ([]byte, error) {
	raw, err := json.Marshal(map[string]any{"workflow_id": workflowID, "revision": revision, "content_digest": contentDigest})
	if err != nil {
		return nil, err
	}
	return CanonicalJSON(raw)
}

// MoleculeID is the identity of the one molecule of (workflow id, revision).
func MoleculeID(workflowID string, revision int64, contentDigest string) (string, error) {
	if workflowID == "" || revision < 1 || !validDigest(contentDigest) {
		return "", errors.New("molecule requires workflow id, revision and content digest")
	}
	resolved, err := moleculeResolution(workflowID, revision, contentDigest)
	if err != nil {
		return "", err
	}
	return Digest(resolved), nil
}

// bindMoleculeTx returns the molecule of (workflow id, revision), creating it
// on first binding and reopening it when a new task joins after it ended. A new
// binding (admission or migration) is refused for a retired workflow; recovery
// of an already bound task (fresh false) is not. A stored molecule that the
// revision's content no longer reproduces is refused.
func (s *Store) bindMoleculeTx(ctx context.Context, tx *sql.Tx, workflowID string, revision int64, fresh bool) (string, error) {
	if fresh {
		var retired int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_status WHERE workflow_id=? AND retired_at IS NOT NULL`, workflowID).Scan(&retired); err != nil {
			return "", err
		}
		if retired > 0 {
			return "", ErrWorkflowRetired
		}
	}
	var digest string
	err := tx.QueryRowContext(ctx, `SELECT content_digest FROM workflows WHERE workflow_id=? AND revision=?`, workflowID, revision).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s revision %d", ErrUnknownRevision, workflowID, revision)
	}
	if err != nil {
		return "", err
	}
	id, err := MoleculeID(workflowID, revision, digest)
	if err != nil {
		return "", err
	}
	var stored, storedDigest string
	var ended sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT molecule_id,content_digest,ended_at FROM molecules WHERE workflow_id=? AND revision=? FOR UPDATE`, workflowID, revision).Scan(&stored, &storedDigest, &ended)
	if err == nil {
		if stored != id || storedDigest != digest {
			return "", ErrMoleculeMismatch
		}
		if ended.Valid && fresh {
			_, err = tx.ExecContext(ctx, `UPDATE molecules SET ended_at=NULL WHERE molecule_id=?`, id)
		}
		return id, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	resolved, err := moleculeResolution(workflowID, revision, digest)
	if err != nil {
		return "", err
	}
	if err = s.putBlob(ctx, tx, resolved); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO molecules (molecule_id,workflow_id,revision,content_digest,created_at) VALUES (?,?,?,?,?)`, id, workflowID, revision, digest, time.Now().UTC())
	return id, err
}

// endMoleculeTx ends a molecule once it has no non-terminal task (FAE-WF-006).
func endMoleculeTx(ctx context.Context, tx *sql.Tx, molecule string) error {
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE molecule_id=? AND state NOT IN ('Done','Retired','Cancelled')`, molecule).Scan(&live); err != nil {
		return err
	}
	if live > 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE molecules SET ended_at=? WHERE molecule_id=? AND ended_at IS NULL`, time.Now().UTC(), molecule)
	return err
}

// BindMolecule retains a single immutable identity for a workflow/revision pair.
func (s *Store) BindMolecule(ctx context.Context, a Audit, id string, revision int64) (string, error) {
	var molecule string
	err := s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) (err error) {
		molecule, err = s.bindMoleculeTx(ctx, tx, id, revision, true)
		return err
	})
	return molecule, err
}

type Molecule struct {
	ID            string     `json:"molecule_id"`
	WorkflowID    string     `json:"workflow_id"`
	Revision      int64      `json:"revision"`
	ContentDigest string     `json:"content_digest"`
	CreatedAt     time.Time  `json:"created_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
}

func scanMolecule(row interface{ Scan(...any) error }) (Molecule, error) {
	var m Molecule
	var ended sql.NullTime
	err := row.Scan(&m.ID, &m.WorkflowID, &m.Revision, &m.ContentDigest, &m.CreatedAt, &ended)
	if ended.Valid {
		at := ended.Time.UTC()
		m.EndedAt = &at
	}
	m.CreatedAt = m.CreatedAt.UTC()
	return m, err
}

const moleculeColumns = `molecule_id,workflow_id,revision,content_digest,created_at,ended_at`

func (s *Store) GetMolecule(ctx context.Context, id string) (Molecule, error) {
	return scanMolecule(s.db.QueryRowContext(ctx, `SELECT `+moleculeColumns+` FROM molecules WHERE molecule_id=?`, id))
}

// TopologyTask is one non-terminal task at its current stage.
type TopologyTask struct {
	ID    string `json:"id"`
	State string `json:"state"`
}
type TopologyMolecule struct {
	Molecule
	Tasks []TopologyTask `json:"tasks"`
}

// Topology is one page of running molecules with the non-terminal tasks at
// each stage (FAE-CP-009). Statements and Rows count what the read touched.
type Topology struct {
	Molecules  []TopologyMolecule `json:"molecules"`
	NextCursor string             `json:"next_cursor,omitempty"`
	Statements int                `json:"statements"`
	Rows       int                `json:"rows"`
}

// Topology pages running molecules (ended_at IS NULL) in id order after the
// cursor and reads only their non-terminal tasks through the task_molecule
// index, and with includeRetired their Retired tasks too (FAE-OPS-011; a
// Retired task's state is "Retired"). It touches the molecules and tasks
// tables only: two statements.
func (s *Store) Topology(ctx context.Context, workflowID, after string, limit, taskLimit int, includeRetired bool) (Topology, error) {
	out := Topology{Molecules: []TopologyMolecule{}}
	if limit < 1 || limit > 500 || taskLimit < 1 || taskLimit > 10000 {
		return out, errors.New("topology limits must be 1..500 molecules and 1..10000 tasks")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+moleculeColumns+` FROM molecules WHERE ended_at IS NULL AND molecule_id>? AND (?='' OR workflow_id=?) ORDER BY molecule_id LIMIT ?`, after, workflowID, workflowID, limit+1)
	if err != nil {
		return out, err
	}
	out.Statements++
	index := map[string]int{}
	args := []any{}
	for rows.Next() {
		m, err := scanMolecule(rows)
		if err != nil {
			rows.Close()
			return out, err
		}
		out.Rows++
		if len(out.Molecules) == limit {
			out.NextCursor = out.Molecules[limit-1].ID
			break
		}
		index[m.ID] = len(out.Molecules)
		out.Molecules = append(out.Molecules, TopologyMolecule{Molecule: m, Tasks: []TopologyTask{}})
		args = append(args, m.ID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(args) == 0 {
		return out, err
	}
	excluded := `('Done','Retired','Cancelled')`
	if includeRetired {
		excluded = `('Done','Cancelled')`
	}
	query := `SELECT molecule_id,task_id,state FROM tasks WHERE molecule_id IN (?` + repeatPlaceholders(len(args)-1) + `) AND state NOT IN ` + excluded + ` ORDER BY molecule_id,task_id LIMIT ?`
	rows, err = s.db.QueryContext(ctx, query, append(args, taskLimit+1)...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Statements++
	n := 0
	for rows.Next() {
		var molecule string
		var t TopologyTask
		if err = rows.Scan(&molecule, &t.ID, &t.State); err != nil {
			return out, err
		}
		out.Rows++
		if n++; n > taskLimit {
			return out, ErrTopologyBound
		}
		m := &out.Molecules[index[molecule]]
		m.Tasks = append(m.Tasks, t)
	}
	return out, rows.Err()
}

func repeatPlaceholders(n int) string {
	b := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		b = append(b, ",?"...)
	}
	return string(b)
}

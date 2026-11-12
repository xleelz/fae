package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

const provenanceColumns = `provenance_id,artifact_id,task_id,molecule_id,kind,dispatch_id,stage,primitive,role,provider,model,session_id,transcript,tree_hash,inputs,commit_ref,execution_kind,workflow_digest,workflow_revision,produced_at`

func scanProvenance(row interface{ Scan(...any) error }) (ProvenanceRecord, error) {
	var p ProvenanceRecord
	var transcript, inputs string
	if err := row.Scan(&p.ID, &p.ArtifactID, &p.TaskID, &p.MoleculeID, &p.Kind, &p.DispatchID, &p.Stage, &p.Primitive, &p.Role, &p.Provider, &p.Model, &p.SessionID, &transcript, &p.Tree, &inputs, &p.CommitRef, &p.ExecutionKind, &p.WorkflowDigest, &p.WorkflowRevision, &p.ProducedAt); err != nil {
		return p, err
	}
	p.ProducedAt = p.ProducedAt.UTC()
	if transcript != "" {
		p.Transcript = &engine.Transcript{}
		if err := json.Unmarshal([]byte(transcript), p.Transcript); err != nil {
			return p, err
		}
	}
	if ref, ok := blobRef(inputs); ok {
		p.inputsRef = &ref
		return p, nil
	}
	return p, json.Unmarshal([]byte(inputs), &p.Inputs)
}

// resolveInputs reads a provenance row's input list stored by reference.
func (s *Store) resolveInputs(ctx context.Context, p *ProvenanceRecord) error {
	if p.inputsRef == nil {
		return nil
	}
	raw, err := s.GetBlob(ctx, *p.inputsRef)
	if err == nil {
		p.inputsRef = nil
		err = json.Unmarshal(raw, &p.Inputs)
	}
	return err
}

func (s *Store) resolveProvenanceMetadata(ctx context.Context, p *ProvenanceRecord) error {
	if p == nil || (p.ReadRoots != nil && p.Reads != nil) {
		return nil
	}
	var stored string
	var err error
	if p.ArtifactID != "" {
		err = s.db.QueryRowContext(ctx, `SELECT provenance FROM artifacts WHERE artifact_id = ? AND task_id = ?`, p.ArtifactID, p.TaskID).Scan(&stored)
	} else if p.DispatchID != "" {
		err = s.db.QueryRowContext(ctx, `SELECT a.provenance FROM artifacts a JOIN provenance pr ON a.artifact_id = pr.artifact_id AND a.task_id = pr.task_id WHERE pr.dispatch_id = ? AND pr.task_id = ? LIMIT 1`, p.DispatchID, p.TaskID).Scan(&stored)
	} else {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if stored == "" {
		return nil
	}
	raw, err := s.ResolveText(ctx, stored)
	if err != nil {
		return err
	}
	var meta struct {
		CheckpointSession     string               `json:"checkpoint_session"`
		CheckpointTree        string               `json:"checkpoint_tree"`
		ResumedFromCheckpoint bool                 `json:"resumed_from_checkpoint"`
		ReadRoots             map[string]string    `json:"read_roots"`
		Reads                 *workflow.ReadPolicy `json:"reads"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil
	}
	if p.ReadRoots == nil && len(meta.ReadRoots) > 0 {
		p.ReadRoots = meta.ReadRoots
	}
	if p.Reads == nil && meta.Reads != nil {
		p.Reads = meta.Reads
	}
	if p.CheckpointSession == "" && meta.CheckpointSession != "" {
		p.CheckpointSession = meta.CheckpointSession
	}
	if p.CheckpointTree == "" && meta.CheckpointTree != "" {
		p.CheckpointTree = meta.CheckpointTree
	}
	if !p.ResumedFromCheckpoint && meta.ResumedFromCheckpoint {
		p.ResumedFromCheckpoint = meta.ResumedFromCheckpoint
	}
	return nil
}

// ProvenanceHop is one artifact on a provenance chain, Depth hops from its start.
type ProvenanceHop struct {
	ProvenanceRecord
	Depth int `json:"depth"`
}

// ProvenanceChain walks from the commit (or landing) artifacts naming sha back
// through each production's input artifacts to artifacts with no inputs, such
// as the spec and its intake (FAE-WF-051). An input resolves to its task's
// latest production of that content no later than the consumer. Every hop is
// one keyed read; the walk is bounded by limit.
func (s *Store) ProvenanceChain(ctx context.Context, sha string, limit int) ([]ProvenanceHop, error) {
	if sha == "" || limit < 1 || limit > 10000 {
		return nil, errors.New("commit and limit 1..10000 required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+provenanceColumns+` FROM provenance WHERE commit_ref=? ORDER BY produced_at,provenance_id LIMIT ?`, sha, limit)
	if err != nil {
		return nil, err
	}
	out := []ProvenanceHop{}
	for rows.Next() {
		p, err := scanProvenance(rows)
		if err == nil {
			err = s.resolveInputs(ctx, &p)
		}
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, ProvenanceHop{p, 0})
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.resolveProvenanceMetadata(ctx, &out[i].ProvenanceRecord); err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	for _, h := range out {
		seen[h.ID] = true
	}
	for n := 0; n < len(out); n++ {
		h := out[n]
		for _, input := range h.Inputs {
			p, err := scanProvenance(s.db.QueryRowContext(ctx, `SELECT `+provenanceColumns+` FROM provenance WHERE task_id=? AND artifact_id=? AND produced_at<=? ORDER BY produced_at DESC,provenance_id DESC LIMIT 1`, h.TaskID, input, h.ProducedAt))
			if err == nil {
				err = s.resolveInputs(ctx, &p)
			}
			if err == nil {
				err = s.resolveProvenanceMetadata(ctx, &p)
			}
			if errors.Is(err, sql.ErrNoRows) {
				return nil, errors.New("provenance chain broken: input " + input + " of " + h.ArtifactID + " has no recorded production")
			}
			if err != nil {
				return nil, err
			}
			if seen[p.ID] {
				continue
			}
			if len(out) >= limit {
				return nil, errors.New("provenance chain exceeds limit")
			}
			seen[p.ID] = true
			out = append(out, ProvenanceHop{p, h.Depth + 1})
		}
	}
	return out, nil
}

// TaskProvenance pages a task's provenance rows in production order.
func (s *Store) TaskProvenance(ctx context.Context, task string, after time.Time, limit int) ([]ProvenanceRecord, error) {
	if task == "" || limit < 1 || limit > 10000 {
		return nil, errors.New("task and limit 1..10000 required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+provenanceColumns+` FROM provenance WHERE task_id=? AND produced_at>? ORDER BY produced_at,provenance_id LIMIT ?`, task, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProvenanceRecord{}
	for rows.Next() {
		p, err := scanProvenance(rows)
		if err == nil {
			err = s.resolveInputs(ctx, &p)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.resolveProvenanceMetadata(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DispatchProvenance reads the provenance rows of the named dispatches.
func (s *Store) DispatchProvenance(ctx context.Context, task string, dispatches []string) ([]ProvenanceRecord, error) {
	out := []ProvenanceRecord{}
	if len(dispatches) == 0 {
		return out, nil
	}
	if task == "" || len(dispatches) > 1000 {
		return nil, errors.New("task and at most 1000 dispatches required")
	}
	args := []any{task}
	for _, d := range dispatches {
		args = append(args, d)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+provenanceColumns+` FROM provenance WHERE task_id=? AND dispatch_id IN (?`+repeatPlaceholders(len(dispatches)-1)+`) ORDER BY produced_at,provenance_id LIMIT 10000`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanProvenance(rows)
		if err == nil {
			err = s.resolveInputs(ctx, &p)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.resolveProvenanceMetadata(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// TransitionRecord is one row of the append-only transition log.
type TransitionRecord struct {
	ID             int64     `json:"id"`
	TaskID         string    `json:"task_id"`
	From           string    `json:"from"`
	To             string    `json:"to"`
	Owner          string    `json:"owner"`
	Attempt        int       `json:"attempt"`
	EvidenceDigest string    `json:"evidence_digest"`
	Note           string    `json:"note"`
	MessageID      string    `json:"message_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// TaskTransitions pages a task's transitions after the id cursor.
func (s *Store) TaskTransitions(ctx context.Context, task string, after int64, limit int) ([]TransitionRecord, error) {
	if task == "" || after < 0 || limit < 1 || limit > 10000 {
		return nil, errors.New("task, nonnegative cursor and limit 1..10000 required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,from_state,to_state,owner,attempt,evidence_digest,note,message_id,created_at FROM transitions WHERE task_id=? AND id>? ORDER BY id LIMIT ?`, task, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TransitionRecord{}
	for rows.Next() {
		var t TransitionRecord
		if err = rows.Scan(&t.ID, &t.TaskID, &t.From, &t.To, &t.Owner, &t.Attempt, &t.EvidenceDigest, &t.Note, &t.MessageID, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.CreatedAt = t.CreatedAt.UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

// DecisionRecord is one row of the append-only decision log.
type DecisionRecord struct {
	ID            string    `json:"id"`
	TaskID        string    `json:"task_id"`
	Kind          string    `json:"kind"`
	PayloadDigest string    `json:"payload_digest"`
	Rationale     string    `json:"rationale"`
	Role          string    `json:"role"`
	SessionID     string    `json:"session_id,omitempty"`
	SupersedesID  string    `json:"supersedes_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// TaskDecisions reads a task's newest decisions, oldest first.
func (s *Store) TaskDecisions(ctx context.Context, task string, limit int) ([]DecisionRecord, error) {
	if task == "" || limit < 1 || limit > 10000 {
		return nil, errors.New("task and limit 1..10000 required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,kind,payload_digest,COALESCE(rationale,''),COALESCE(role,''),COALESCE(session_id,''),COALESCE(supersedes_id,''),created_at FROM decisions WHERE task_id=? ORDER BY created_at DESC,id DESC LIMIT ?`, task, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DecisionRecord{}
	for rows.Next() {
		var d DecisionRecord
		if err = rows.Scan(&d.ID, &d.TaskID, &d.Kind, &d.PayloadDigest, &d.Rationale, &d.Role, &d.SessionID, &d.SupersedesID, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.CreatedAt = d.CreatedAt.UTC()
		out = append(out, d)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// ArtifactContent reads a task's artifact content by its content address.
func (s *Store) ArtifactContent(ctx context.Context, task, artifact string) (json.RawMessage, error) {
	var digest string
	if err := s.db.QueryRowContext(ctx, `SELECT content_digest FROM artifacts WHERE artifact_id=? AND task_id=?`, artifact, task).Scan(&digest); err != nil {
		return nil, err
	}
	var size int64
	if err := s.db.QueryRowContext(ctx, `SELECT size FROM blobs WHERE digest=?`, digest).Scan(&size); err != nil {
		return nil, err
	}
	return s.GetBlob(ctx, BlobRef{Digest: digest, Size: size})
}

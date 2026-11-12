package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/engine"
	"time"
)

var ErrConflict = errors.New("stale job version or immutable binding changed")
var ErrIdempotencyConflict = errors.New("idempotency key reused with different request")
var errReplay = errors.New("idempotent replay")

type JobSnapshot struct {
	ID               string          `json:"id"`
	Version          int64           `json:"version"`
	WorkflowID       string          `json:"workflow_id"`
	WorkflowRevision int64           `json:"workflow_revision"`
	TargetID         string          `json:"target_id"`
	TargetRevision   int64           `json:"target_revision"`
	State            string          `json:"state"`
	Payload          json.RawMessage `json:"payload"`
	// MoleculeID is the bound molecule, assigned by the plane at admission.
	MoleculeID string  `json:"molecule_id,omitempty"`
	Events     []Event `json:"-"`
	// Decisions commit with the snapshot that applies them (decision.go).
	Decisions []Decision `json:"-"`
	// replayed marks AdmitJob's original response to an idempotent retry.
	replayed bool
}

// Replayed reports an admission retry answered with its original response,
// which may be older than the job's current checkpoint.
func (j JobSnapshot) Replayed() bool { return j.replayed }

type Event struct {
	ParentTask string          `json:"parent_task"`
	TreeRoot   string          `json:"tree_root"`
	RunID      string          `json:"run_id,omitempty"`
	WorkflowID string          `json:"workflow_id,omitempty"`
	Sequence   int64           `json:"sequence"`
	Kind       string          `json:"kind"`
	JobID      string          `json:"job_id"`
	Payload    json.RawMessage `json:"payload"`
	Timestamp  time.Time       `json:"timestamp"`
}

func validateJob(j JobSnapshot) error {
	if j.ID == "" || len(j.ID) > MaxTaskIDBytes || j.WorkflowID == "" || j.WorkflowRevision < 1 || j.TargetID == "" || j.TargetRevision < 1 || j.State == "" {
		return errors.New("job identity, pinned revisions and state required")
	}
	if !json.Valid(j.Payload) {
		return errors.New("job payload must be JSON")
	}
	return nil
}

const jobColumns = `job_id,version,workflow_id,workflow_revision,target_id,target_revision,state,payload_digest,payload_size,molecule_id`

func scanJob(row interface{ Scan(...any) error }) (JobSnapshot, BlobRef, error) {
	var j JobSnapshot
	var r BlobRef
	e := row.Scan(&j.ID, &j.Version, &j.WorkflowID, &j.WorkflowRevision, &j.TargetID, &j.TargetRevision, &j.State, &r.Digest, &r.Size, &j.MoleculeID)
	return j, r, e
}
func (s *Store) GetJob(ctx context.Context, id string) (JobSnapshot, error) {
	j, r, e := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE job_id=?`, id))
	if e != nil {
		return j, e
	}
	j.Payload, e = s.GetBlob(ctx, r)
	return j, e
}
func (s *Store) ListJobs(ctx context.Context, limit int) ([]JobSnapshot, error) {
	if limit < 1 || limit > 100000 {
		return nil, errors.New("job limit must be 1..100000")
	}
	rows, e := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs ORDER BY job_id LIMIT ?`, limit+1)
	if e != nil {
		return nil, e
	}
	var jobs []JobSnapshot
	var refs []BlobRef
	for rows.Next() {
		j, r, e := scanJob(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		jobs = append(jobs, j)
		refs = append(refs, r)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if len(jobs) > limit {
		return nil, errors.New("job listing exceeds limit; refusing incomplete snapshot")
	}
	for i := range jobs {
		jobs[i].Payload, e = s.GetBlob(ctx, refs[i])
		if e != nil {
			return nil, e
		}
	}
	return jobs, nil
}

// AdmitJob stores the original response for retries, even after the job advances.
// Callers must validate engine content before this generic persistence boundary.
func (s *Store) AdmitJob(ctx context.Context, a Audit, key string, j JobSnapshot) (JobSnapshot, error) {
	if e := validateJob(j); e != nil {
		return j, e
	}
	if len(key) > 255 {
		return j, errors.New("idempotency key too long")
	}
	j.Version, j.MoleculeID = 1, ""
	raw, e := json.Marshal(j)
	if e != nil {
		return j, e
	}
	requestContent := raw
	if len(a.Inputs) > 0 {
		requestContent = a.Inputs
	}
	requestHash, _, e := ContentHash(requestContent)
	if e != nil {
		return j, e
	}
	var replay BlobRef
	e = s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if e := lockEvents(ctx, tx); e != nil {
			return e
		}
		if key != "" {
			var hash string
			e := tx.QueryRowContext(ctx, `SELECT request_digest,response_digest,response_size FROM request_keys WHERE request_key=?`, key).Scan(&hash, &replay.Digest, &replay.Size)
			if e == nil {
				if hash != requestHash {
					return ErrIdempotencyConflict
				}
				return errReplay
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
		}
		if e := checkJobAdapterTx(ctx, tx, j); e != nil {
			return e
		}
		if e := s.admitJobTx(ctx, tx, a, &j); e != nil {
			return e
		}
		if key != "" {
			response, e := json.Marshal(j)
			if e != nil {
				return e
			}
			if e = s.putBlob(ctx, tx, response); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, `INSERT INTO request_keys (request_key,request_digest,response_digest,response_size) VALUES (?,?,?,?)`, key, requestHash, Digest(response), len(response)); e != nil {
				return e
			}
		}
		return s.appendEvents(ctx, tx, j.ID, j.Events, "task_admitted")
	})
	if errors.Is(e, errReplay) {
		raw, e = s.GetBlob(ctx, replay)
		if e == nil {
			e = json.Unmarshal(raw, &j)
		}
		j.replayed = true
	}
	return j, e
}

// SaveJob is a version CAS; snapshots and their events/audit commit together.
// Runtime execution must happen outside this retryable SQL-only transaction.
func (s *Store) SaveJob(ctx context.Context, a Audit, j JobSnapshot, expected int64) (JobSnapshot, error) {
	if e := validateJob(j); e != nil {
		return j, e
	}
	if expected < 1 {
		return j, ErrConflict
	}
	j.Version = expected + 1
	e := s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if e := lockEvents(ctx, tx); e != nil {
			return e
		}
		old, _, e := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE job_id=? FOR UPDATE`, j.ID))
		if e != nil {
			return e
		}
		if old.Version != expected || old.WorkflowID != j.WorkflowID || old.WorkflowRevision != j.WorkflowRevision || old.TargetID != j.TargetID || old.TargetRevision != j.TargetRevision {
			return ErrConflict
		}
		if j.State == "Retired" {
			if old.State != "Escalated" {
				return errors.New("retire requires Escalated")
			}
			var envelope struct {
				Job engine.Job `json:"job"`
			}
			if e := json.Unmarshal(j.Payload, &envelope); e != nil {
				return e
			}
			class, reference, e := engine.ParseRetirement(envelope.Job.Reason)
			if e != nil {
				return e
			}
			if class == "superseded-by" {
				if e = s.transferRetiredEdges(ctx, tx, j.ID, reference, a.Actor); e != nil {
					return e
				}
			}
		}
		if j.MoleculeID = old.MoleculeID; j.MoleculeID == "" {
			// A job admitted before molecules were bound joins its revision's molecule.
			if j.MoleculeID, e = s.bindMoleculeTx(ctx, tx, j.WorkflowID, j.WorkflowRevision, false); e != nil {
				return e
			}
		}
		payload, e := CanonicalJSON(j.Payload)
		if e != nil {
			return e
		}
		if e = s.putBlob(ctx, tx, payload); e != nil {
			return e
		}
		if e = s.inject("job"); e != nil {
			return e
		}
		result, e := tx.ExecContext(ctx, `UPDATE jobs SET version=?,state=?,payload_digest=?,payload_size=?,molecule_id=? WHERE job_id=? AND version=?`, j.Version, j.State, Digest(payload), len(payload), j.MoleculeID, j.ID, expected)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrConflict
		}
		if e = s.recordJobTx(ctx, tx, a, j, j.MoleculeID); e != nil {
			return e
		}
		if e = s.inject("backlog"); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE backlog_items SET state=?,updated_at=? WHERE item_id=?`, j.State, time.Now().UTC(), j.ID); e != nil {
			return e
		}
		for _, d := range j.Decisions {
			if e = s.insertDecision(ctx, tx, j.ID, d); e != nil {
				return e
			}
		}
		if e = s.inject("events"); e != nil {
			return e
		}
		return s.appendEvents(ctx, tx, j.ID, j.Events, "job_updated")
	})
	if e == nil {
		j.Decisions = nil // committed with this version; never re-applied by a later save
	}
	return j, e
}

// admitJobTx binds a new job to its revision's molecule (refusing a retired or
// unknown workflow revision, FAE-WF-006, -007), inserts its snapshot and
// projects its task records, all in the admission transaction.
func (s *Store) admitJobTx(ctx context.Context, tx *sql.Tx, a Audit, j *JobSnapshot) error {
	molecule, e := s.bindMoleculeTx(ctx, tx, j.WorkflowID, j.WorkflowRevision, true)
	if e != nil {
		return e
	}
	j.MoleculeID = molecule
	payload, e := CanonicalJSON(j.Payload)
	if e != nil {
		return e
	}
	if e = s.putBlob(ctx, tx, payload); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO jobs (`+jobColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`, j.ID, j.Version, j.WorkflowID, j.WorkflowRevision, j.TargetID, j.TargetRevision, j.State, Digest(payload), len(payload), molecule); e != nil {
		return e
	}
	return s.recordJobTx(ctx, tx, a, *j, molecule)
}
func lockEvents(ctx context.Context, tx *sql.Tx) error {
	var n int64
	return tx.QueryRowContext(ctx, `SELECT sequence FROM event_clock WHERE id=1 FOR UPDATE`).Scan(&n)
}
func (s *Store) appendEvents(ctx context.Context, tx *sql.Tx, id string, events []Event, fallback string) error {
	if len(events) == 0 {
		events = []Event{{Kind: fallback, Payload: json.RawMessage(`{}`)}}
	}
	var seq int64
	if e := tx.QueryRowContext(ctx, `SELECT sequence FROM event_clock WHERE id=1`).Scan(&seq); e != nil {
		return e
	}
	metadata := map[string]eventMetadata{}
	for _, event := range events {
		eventJob := id
		if event.JobID != "" && event.JobID != id && event.Kind == "change-request" {
			var p struct {
				Request struct{ ID, Source, Target string }
			}
			if json.Unmarshal(event.Payload, &p) != nil || p.Request.Source != id || p.Request.Target != event.JobID {
				return errors.New("invalid cross-task change-request event")
			}
			eventJob = event.JobID
			var body struct {
				Request json.RawMessage `json:"request"`
			}
			if err := json.Unmarshal(event.Payload, &body); err != nil {
				return err
			}
			key := "change-request/" + Digest([]byte(p.Request.Target)) + "/" + Digest([]byte(p.Request.ID))
			if err := s.putMarkerTx(ctx, tx, key, body.Request); err != nil {
				return err
			}
		}
		if event.JobID != "" && event.JobID != id && event.Kind == "child-admitted" {
			var link struct {
				ChildTask  string `json:"child_task"`
				ParentTask string `json:"parent_task"`
			}
			if json.Unmarshal(event.Payload, &link) != nil || link.ChildTask != id || link.ParentTask != event.JobID {
				return errors.New("invalid cross-task child admission event")
			}
			eventJob = event.JobID
		}
		if event.Kind == "" || len(event.Kind) > 255 {
			return errors.New("event kind required")
		}
		if len(event.Payload) == 0 {
			event.Payload = json.RawMessage(`{}`)
		}
		payload, e := CanonicalJSON(event.Payload)
		if e != nil {
			return e
		}
		if eventJob != "" {
			m, ok := metadata[eventJob]
			if !ok {
				m, e = taskEventMetadata(ctx, tx, eventJob)
				if e != nil {
					return e
				}
				metadata[eventJob] = m
			}
			innerStored, innerErr := s.inlineOrRef(ctx, tx, payload)
			if innerErr != nil {
				return innerErr
			}
			payload, e = json.Marshal(storedTaskEvent{Version: 1, Metadata: m, Payload: json.RawMessage(innerStored)})
			if e != nil {
				return e
			}
		}
		// An oversized payload is stored by reference (FAE-CP-012).
		stored, e := s.inlineOrRef(ctx, tx, payload)
		if e != nil {
			return e
		}
		seq++
		if _, e = tx.ExecContext(ctx, `INSERT INTO events (sequence,kind,job_id,payload,created_at) VALUES (?,?,?,?,?)`, seq, event.Kind, eventJob, stored, time.Now().UTC()); e != nil {
			return e
		}
	}
	_, e := tx.ExecContext(ctx, `UPDATE event_clock SET sequence=? WHERE id=1`, seq)
	return e
}
func (s *Store) Events(ctx context.Context, after int64, limit int) ([]Event, error) {
	return s.readEvents(ctx, "", after, limit)
}
func (s *Store) VisibleEvents(ctx context.Context, after int64, limit int) ([]Event, error) {
	return s.readEvents(ctx, "", after, limit, true)
}
func (s *Store) JobEvents(ctx context.Context, id string, after int64, limit int) ([]Event, error) {
	if id == "" {
		return nil, errors.New("job id required")
	}
	return s.readEvents(ctx, id, after, limit)
}
func (s *Store) readEvents(ctx context.Context, id string, after int64, limit int, hideRetired ...bool) ([]Event, error) {
	if after < 0 || limit < 1 || limit > 100000 {
		return nil, errors.New("invalid event range")
	}
	query := `SELECT sequence,kind,job_id,payload,created_at FROM events WHERE sequence>?`
	args := []any{after}
	if id != "" {
		query += ` AND job_id=?`
		args = append(args, id)
	}
	if len(hideRetired) > 0 && hideRetired[0] {
		query += ` AND NOT EXISTS (SELECT 1 FROM jobs WHERE jobs.job_id=events.job_id AND jobs.state='Retired')`
	}
	query += ` ORDER BY sequence LIMIT ?`
	args = append(args, limit)
	rows, e := s.db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var ev Event
		if e = rows.Scan(&ev.Sequence, &ev.Kind, &ev.JobID, &ev.Payload, &ev.Timestamp); e != nil {
			return nil, e
		}
		events = append(events, ev)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return s.restoreEventMetadata(ctx, events)
}

// PutMarker replaces a durable daemon marker. A schedule marker projects its
// schedules, and each projection its record family, in the same transaction.
func (s *Store) PutMarker(ctx context.Context, a Audit, name string, payload json.RawMessage, projections ...Projection) error {
	if name == "" || len(name) > 255 {
		return errors.New("marker name required")
	}
	canonical, e := CanonicalJSON(payload)
	if e != nil {
		return e
	}
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if e := s.putBlob(ctx, tx, canonical); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `REPLACE INTO daemon_markers (name,payload_digest,payload_size) VALUES (?,?,?)`, name, Digest(canonical), len(canonical)); e != nil {
			return e
		}
		if e := s.projectSchedulesTx(ctx, tx, a, name, canonical); e != nil {
			return e
		}
		now := time.Now().UTC()
		for _, p := range projections {
			if e := p.projectTx(ctx, s, tx, canonical, now); e != nil {
				return e
			}
		}
		return nil
	})
}
func (s *Store) GetMarker(ctx context.Context, name string) (json.RawMessage, error) {
	var r BlobRef
	e := s.db.QueryRowContext(ctx, `SELECT payload_digest,payload_size FROM daemon_markers WHERE name=?`, name).Scan(&r.Digest, &r.Size)
	if e != nil {
		return nil, e
	}
	return s.GetBlob(ctx, r)
}
func (s *Store) RecordMessage(ctx context.Context, id, text string) error {
	_, e := s.PostMessage(ctx, MessageInput{ID: id, Text: text})
	return e
}
func (s *Store) RequireMessage(ctx context.Context, id string) error {
	if e := s.RequireScheduleAuthority(ctx, id); e != nil {
		return e
	}
	var created time.Time
	if e := s.db.QueryRowContext(ctx, `SELECT created_at FROM operator_messages WHERE message_id=?`, id).Scan(&created); e != nil {
		return e
	}
	age := time.Since(created)
	if age < 0 || age > s.options.MessageMaxAge {
		return fmt.Errorf("authorizing message %q outside maximum age %s", id, s.options.MessageMaxAge)
	}
	return nil
}

// LookupAdmission resolves the original response using the original RPC input,
// before callers resolve mutable defaults such as latest workflow revision/HEAD.
func (s *Store) LookupAdmission(ctx context.Context, key string, request json.RawMessage) (JobSnapshot, bool, error) {
	var j JobSnapshot
	if key == "" {
		return j, false, nil
	}
	digest, _, e := ContentHash(request)
	if e != nil {
		return j, false, e
	}
	var recorded string
	var ref BlobRef
	e = s.db.QueryRowContext(ctx, `SELECT request_digest,response_digest,response_size FROM request_keys WHERE request_key=?`, key).Scan(&recorded, &ref.Digest, &ref.Size)
	if errors.Is(e, sql.ErrNoRows) {
		return j, false, nil
	}
	if e != nil {
		return j, false, e
	}
	if recorded != digest {
		return j, false, ErrIdempotencyConflict
	}
	raw, e := s.GetBlob(ctx, ref)
	if e != nil {
		return j, false, e
	}
	e = json.Unmarshal(raw, &j)
	return j, e == nil, e
}

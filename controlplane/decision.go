package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ErrDuplicateDecision refuses a second row for a decision already recorded.
var ErrDuplicateDecision = errors.New("decision already recorded")

// DecisionRow is one recorded decisions row (FAE-CP-011) as read back. Writers
// supply the existing Decision shape; for an operator HumanDecision the role is
// "operator", the session is the decision request it disposed, and the
// canonical payload cites the authorizing message.
type DecisionRow struct {
	ID         string          `json:"id"`
	TaskID     string          `json:"task_id"`
	Kind       string          `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	Rationale  string          `json:"rationale,omitempty"`
	Role       string          `json:"role,omitempty"`
	Session    string          `json:"session,omitempty"`
	Supersedes string          `json:"supersedes,omitempty"`
	Created    time.Time       `json:"created_at"`
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// insertDecision writes one decision inside the caller's transaction, so the
// row commits with the transition and projection it caused (FAE-WF-022). A
// recorded id is never overwritten.
func (s *Store) insertDecision(ctx context.Context, tx *sql.Tx, task string, d Decision) error {
	if d.ID == "" || len(d.ID) > 255 || task == "" || len(task) > MaxTaskIDBytes || d.Kind == "" || len(d.Kind) > 255 || len(d.Role) > 255 || len(d.SessionID) > 3072 || len(d.SupersedesID) > 255 {
		return errors.New("decision identity, task, kind and bounded references required")
	}
	payload, e := CanonicalJSON(d.Payload)
	if e != nil {
		return e
	}
	var one int
	switch e = tx.QueryRowContext(ctx, `SELECT 1 FROM decisions WHERE id=? FOR UPDATE`, d.ID).Scan(&one); {
	case e == nil:
		return ErrDuplicateDecision
	case !errors.Is(e, sql.ErrNoRows):
		return e
	}
	if e = s.putBlob(ctx, tx, payload); e != nil {
		return e
	}
	rationale, e := s.inlineOrRef(ctx, tx, []byte(d.Rationale))
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO decisions (id,task_id,kind,payload_digest,rationale,role,session_id,supersedes_id,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, d.ID, task, d.Kind, Digest(payload), nullable(rationale), nullable(d.Role), nullable(d.SessionID), nullable(d.SupersedesID), time.Now().UTC())
	return e
}

// TaskDecisionPayloads reads one task's decisions with payloads in recording order (keyed read).
func (s *Store) TaskDecisionPayloads(ctx context.Context, task string, limit int) ([]DecisionRow, error) {
	if task == "" || limit < 1 || limit > 10000 {
		return nil, errors.New("task and a limit of 1..10000 required")
	}
	rows, e := s.db.QueryContext(ctx, `SELECT d.id,d.task_id,d.kind,d.payload_digest,b.size,d.rationale,d.role,d.session_id,d.supersedes_id,d.created_at FROM decisions d JOIN blobs b ON b.digest=d.payload_digest WHERE d.task_id=? ORDER BY d.created_at,d.id LIMIT ?`, task, limit)
	if e != nil {
		return nil, e
	}
	type row struct {
		d   DecisionRow
		ref BlobRef
	}
	found := []row{}
	for rows.Next() {
		var r row
		var rationale, role, session, supersedes sql.NullString
		if e = rows.Scan(&r.d.ID, &r.d.TaskID, &r.d.Kind, &r.ref.Digest, &r.ref.Size, &rationale, &role, &session, &supersedes, &r.d.Created); e != nil {
			rows.Close()
			return nil, e
		}
		r.d.Rationale, r.d.Role, r.d.Session, r.d.Supersedes = rationale.String, role.String, session.String, supersedes.String
		found = append(found, r)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return nil, e
	}
	rows.Close()
	out := make([]DecisionRow, 0, len(found))
	for _, r := range found {
		if r.d.Payload, e = s.GetBlob(ctx, r.ref); e != nil {
			return nil, e
		}
		out = append(out, r.d)
	}
	return out, nil
}

package controlplane

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/domain"
	"time"
)

type Task struct {
	ID             string          `json:"id"`
	RunID          string          `json:"run_id"`
	MoleculeID     string          `json:"molecule_id"`
	TargetID       string          `json:"target_id"`
	TargetRevision int64           `json:"target_revision"`
	State          string          `json:"state"`
	Attempt        int             `json:"attempt"`
	AttemptLimit   int             `json:"attempt_limit"`
	Priority       int             `json:"priority"`
	Disposition    json.RawMessage `json:"disposition"`
}

func (s *Store) CreateTask(ctx context.Context, a Audit, t Task) error {
	if t.ID == "" || t.RunID == "" || !validDigest(t.MoleculeID) || t.TargetID == "" || t.TargetRevision < 1 || t.State != "Pending" || t.Attempt != 0 || t.AttemptLimit < 1 || t.Priority < 10 {
		return errors.New("invalid new task or unbound molecule/target")
	}
	disposition, e := CanonicalJSON(t.Disposition)
	if e != nil {
		return fmt.Errorf("disposition: %w", e)
	}
	var d domain.Disposition
	decoder := json.NewDecoder(bytes.NewReader(disposition))
	decoder.DisallowUnknownFields()
	if e := decoder.Decode(&d); e != nil {
		return fmt.Errorf("disposition: %w", e)
	}
	if d.Mode != domain.Local && d.Mode != domain.Normal {
		return errors.New("unknown disposition mode")
	}
	if d.Mode == domain.Local && (d.Live || d.TicketRoom != "" || d.Destination != "" || d.PushNamespace != "") {
		return errors.New("local disposition cannot request external effects")
	}
	if d.Mode == domain.Normal && d.Live && (d.TicketRoom == "" || d.Destination == "" || d.PushNamespace == "") {
		return errors.New("live disposition requires pinned external destinations")
	}
	if len(disposition) > s.options.InlineLimit {
		return errors.New("disposition exceeds inline limit")
	}
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		var exists int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM molecules WHERE molecule_id=? AND ended_at IS NULL`, t.MoleculeID).Scan(&exists); e != nil {
			return e
		}
		if exists != 1 {
			return errors.New("molecule missing or ended")
		}
		now := time.Now().UTC()
		_, e := tx.ExecContext(ctx, `INSERT INTO tasks (task_id,run_id,molecule_id,target_id,target_revision,state,attempt,attempt_limit,priority,disposition,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, t.ID, t.RunID, t.MoleculeID, t.TargetID, t.TargetRevision, t.State, t.Attempt, t.AttemptLimit, t.Priority, string(disposition), now, now)
		return e
	})
}

const taskColumns = `task_id,run_id,molecule_id,target_id,target_revision,state,attempt,attempt_limit,priority,disposition`

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var disposition []byte
	e := row.Scan(&t.ID, &t.RunID, &t.MoleculeID, &t.TargetID, &t.TargetRevision, &t.State, &t.Attempt, &t.AttemptLimit, &t.Priority, &disposition)
	t.Disposition = disposition
	return t, e
}
func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE task_id=?`, id))
}
func (s *Store) ListTasks(ctx context.Context, run string, limit int) ([]Task, error) {
	if run == "" || limit < 1 || limit > 1000 {
		return nil, errors.New("run and limit 1..1000 required")
	}
	rows, e := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE run_id=? ORDER BY priority DESC,task_id LIMIT ?`, run, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, e := scanTask(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type Transition struct {
	TaskID         string
	From           string
	To             string
	Owner          string
	EvidenceDigest string
	Note           string
	Decision       *Decision
}
type Decision struct {
	ID           string
	Kind         string
	Payload      json.RawMessage
	Rationale    string
	Role         string
	SessionID    string
	SupersedesID string
}

// Transition requires the pure workflow validator to authorize the edge first. It performs
// a compare-and-swap against durable state and refuses terminal/stale projections.
func (s *Store) Transition(ctx context.Context, a Audit, t Transition, validate func(Task, Transition) error) error {
	if validate == nil || t.TaskID == "" || t.From == "" || t.To == "" || t.Owner == "" || !validDigest(t.EvidenceDigest) {
		return errors.New("transition requires validator, identity and evidence")
	}
	if len(t.Note) > s.options.InlineLimit {
		return errors.New("transition note exceeds inline limit")
	}
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		task, e := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE task_id=? FOR UPDATE`, t.TaskID))
		if e != nil {
			return e
		}
		if task.State != t.From {
			return errors.New("stale task state")
		}
		if task.State == "Done" || task.State == "Retired" || task.State == "Cancelled" {
			return errors.New("terminal task cannot transition")
		}
		if e = validate(task, t); e != nil {
			return e
		}
		var present int
		if e = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE task_id=? AND artifact_id=?`, t.TaskID, t.EvidenceDigest).Scan(&present); e != nil {
			return e
		}
		if present != 1 {
			return errors.New("transition evidence is not recorded for task")
		}
		now := time.Now().UTC()
		if _, e = tx.ExecContext(ctx, `INSERT INTO transitions (task_id,from_state,to_state,owner,attempt,evidence_digest,note,message_id,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, t.TaskID, t.From, t.To, t.Owner, task.Attempt, t.EvidenceDigest, t.Note, a.MessageID, now); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE tasks SET state=?,updated_at=? WHERE task_id=? AND state=?`, t.To, now, t.TaskID, t.From); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE backlog_items SET state=?,updated_at=? WHERE item_id=?`, t.To, now, t.TaskID); e != nil {
			return e
		}
		if t.Decision != nil {
			d := t.Decision
			hash, content, e := ContentHash(d.Payload)
			if e != nil {
				return e
			}
			if e = s.putBlob(ctx, tx, content); e != nil {
				return e
			}
			_, e = tx.ExecContext(ctx, `INSERT INTO decisions (id,task_id,kind,payload_digest,rationale,role,session_id,supersedes_id,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, d.ID, t.TaskID, d.Kind, hash, d.Rationale, d.Role, d.SessionID, d.SupersedesID, now)
			if e != nil {
				return e
			}
		}
		return nil
	})
}

func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32
}

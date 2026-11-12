package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"syscall"
	"time"
)

// Ping checks the plane answers within ctx's deadline (FAE-CP-006).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Kill ends an unresponsive child at once; its exit is the restart trigger.
func (s *Server) Kill() error {
	select {
	case <-s.done:
		return nil
	default:
	}
	if e := s.cmd.Process.Signal(syscall.SIGKILL); e != nil {
		select {
		case <-s.done:
			return nil
		default:
			return e
		}
	}
	return nil
}

// Consistency is the plane's own re-validation after an outage: the event
// clock covers every event, and every active bound job's task row, and its
// backlog row when it has one, records its state (all three are written in
// one transaction, so a difference is damage).
type Consistency struct {
	EventClock int64    `json:"event_clock"`
	LastEvent  int64    `json:"last_event"`
	Diverged   []string `json:"diverged_tasks"`
}

// Consistent reports a plane the daemon may resume on.
func (c Consistency) Consistent() bool { return c.EventClock >= c.LastEvent && len(c.Diverged) == 0 }

// consistencyPage is the most diverged tasks one read of the check returns;
// the check reads page after page, by task id, until one comes back short.
const consistencyPage = 1000

// CheckConsistency reads the invariants by key and index: no snapshot is
// read. Every active task is checked, a page of diverged tasks at a time.
func (s *Store) CheckConsistency(ctx context.Context) (Consistency, error) {
	return s.CheckConsistencyBounded(ctx, 0)
}

// CheckConsistencyBounded is CheckConsistency with each of its reads bounded
// by read on its own (no bound but ctx's when read is zero), so a plane with
// many diverged tasks is still checked whole while a hung plane fails.
func (s *Store) CheckConsistencyBounded(ctx context.Context, read time.Duration) (Consistency, error) {
	var c Consistency
	bounded := func() (context.Context, context.CancelFunc) {
		if read > 0 {
			return context.WithTimeout(ctx, read)
		}
		return context.WithCancel(ctx)
	}
	one, cancel := bounded()
	err := s.db.QueryRowContext(one, `SELECT sequence FROM event_clock WHERE id=1`).Scan(&c.EventClock)
	if err == nil {
		err = s.db.QueryRowContext(one, `SELECT COALESCE(MAX(sequence),0) FROM events`).Scan(&c.LastEvent)
	}
	cancel()
	if err != nil {
		return c, err
	}
	c.Diverged = []string{}
	for after := ""; ; {
		one, cancel := bounded()
		page, err := s.divergedTasksAfter(one, after)
		cancel()
		if err != nil {
			return c, err
		}
		c.Diverged = append(c.Diverged, page...)
		if len(page) < consistencyPage {
			return c, nil
		}
		after = page[len(page)-1]
	}
}

// divergedTasksAfter is one page of the active bound jobs, after the task id
// after, whose task row is missing or records another state, or whose backlog
// row records another state.
func (s *Store) divergedTasksAfter(ctx context.Context, after string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT j.job_id FROM jobs j LEFT JOIN tasks t ON t.task_id=j.job_id LEFT JOIN backlog_items b ON b.item_id=j.job_id WHERE j.state NOT IN `+terminalStates+` AND j.molecule_id<>'' AND j.job_id>? AND (t.task_id IS NULL OR t.state<>j.state OR (b.item_id IS NOT NULL AND b.state<>j.state)) ORDER BY j.job_id LIMIT ?`, after, consistencyPage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TaskRowDivergence describes how one job's task and backlog rows differ
// from the state its snapshot row records ("" when they agree), the
// difference CheckConsistency finds; a missing task row is ErrTaskRowMissing.
func (s *Store) TaskRowDivergence(ctx context.Context, id string) (string, error) {
	var job string
	var task, backlog sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT j.state,t.state,b.state FROM jobs j LEFT JOIN tasks t ON t.task_id=j.job_id LEFT JOIN backlog_items b ON b.item_id=j.job_id WHERE j.job_id=?`, id).Scan(&job, &task, &backlog)
	if err != nil {
		return "", err
	}
	if !task.Valid {
		return "", ErrTaskRowMissing
	}
	return describeRowDivergence(job, task.String, backlog), nil
}

func describeRowDivergence(job, task string, backlog sql.NullString) string {
	var parts []string
	if task != job {
		parts = append(parts, "task row state "+task)
	}
	if backlog.Valid && backlog.String != job {
		parts = append(parts, "backlog row state "+backlog.String)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " and ") + " differ from the snapshot's state " + job
}

// ErrTaskRowMissing is a job whose task row is gone: its records can't be
// rebuilt from the snapshot without duplicating its transitions.
var ErrTaskRowMissing = errors.New("the task row is missing")

// RepairTaskRows sets one job's task row state, and its backlog row state
// when it has one, to the state its snapshot row records at version, in one
// audited transaction, and returns what it changed (nothing when they
// already agree). It repairs only what CheckConsistency compares; a missing
// task row is refused (ErrTaskRowMissing) and a changed job is ErrConflict.
func (s *Store) RepairTaskRows(ctx context.Context, a Audit, id string, version int64) (string, error) {
	var repaired string
	err := s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		repaired = ""
		var job string
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT state,version FROM jobs WHERE job_id=? FOR UPDATE`, id).Scan(&job, &current); err != nil {
			return err
		}
		if current != version {
			return ErrConflict
		}
		var task string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE task_id=? FOR UPDATE`, id).Scan(&task); errors.Is(err, sql.ErrNoRows) {
			return ErrTaskRowMissing
		} else if err != nil {
			return err
		}
		var backlog sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT state FROM backlog_items WHERE item_id=? FOR UPDATE`, id).Scan(&backlog); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		repaired = describeRowDivergence(job, task, backlog)
		now := time.Now().UTC()
		if task != job {
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state=?,updated_at=? WHERE task_id=?`, job, now, id); err != nil {
				return err
			}
		}
		if backlog.Valid && backlog.String != job {
			if _, err := tx.ExecContext(ctx, `UPDATE backlog_items SET state=?,updated_at=? WHERE item_id=?`, job, now, id); err != nil {
				return err
			}
		}
		return nil
	})
	return repaired, err
}

// storedRevisionPage is the most workflow revisions one StoredRevisionsAfter
// read lists.
const storedRevisionPage = 500

// StoredRevisionsAfter lists one page of stored workflow revisions, in
// workflow and revision order, after the revision (id, revision); a page
// shorter than StoredRevisionPage() is the last. Unlike ListWorkflows it has
// no total bound, so a caller can visit every stored revision.
func (s *Store) StoredRevisionsAfter(ctx context.Context, id string, revision int64) ([]WorkflowListing, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT w.workflow_id,w.revision,COALESCE(w.parent_revision,0),w.name,w.content_digest,w.created_by,w.created_at,st.retired_at FROM workflows w LEFT JOIN workflow_status st ON st.workflow_id=w.workflow_id WHERE w.workflow_id>? OR (w.workflow_id=? AND w.revision>?) ORDER BY w.workflow_id,w.revision LIMIT ?`, id, id, revision, storedRevisionPage)
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
		out = append(out, r)
	}
	return out, rows.Err()
}

// StoredRevisionPage is the page size of StoredRevisionsAfter.
func StoredRevisionPage() int { return storedRevisionPage }

// ActiveJobVersions reads the version of every active job through the state
// index, without any snapshot.
func (s *Store) ActiveJobVersions(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id,version FROM jobs WHERE state NOT IN `+terminalStates+` ORDER BY job_id LIMIT 100001`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var v int64
		if err = rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		if out[id] = v; len(out) > 100000 {
			return nil, errors.New("active job bound exceeded")
		}
	}
	return out, rows.Err()
}

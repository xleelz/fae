package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// TerminalFilter narrows a terminal page. States, Target, Workflow and
// Molecule are the job row's own columns; RunID, Mode and MinPriority are the
// task row's. A row whose task columns cannot reproduce its Board row (see
// TerminalRow.Columns) passes the task-column filters, so the caller filters
// it after decoding its snapshot.
type TerminalFilter struct {
	// States are terminal states; empty means all of them.
	States                     []string
	Target, Workflow, Molecule string
	RunID                      string
	// Mode matches case-insensitively, as Board's filter does.
	Mode        string
	MinPriority int
}

// TerminalRow is one finished task's Board row read from the job and task
// columns. Columns reports that the task row holds the job's execution kind,
// reason and escalation kind and the admission's run, mode and priority
// exactly as the snapshot does (BoardColumns); otherwise Payload holds the
// job snapshot and the caller decodes the row from it.
type TerminalRow struct {
	JobSnapshot
	Columns                                        bool
	ExecutionKind, RunID, Mode, Reason, Escalation string
	Priority                                       int
}

// TerminalRowsAfter pages the terminal jobs after a job id, in id order, in
// one statement with every filter in it (FAE-CP-009): per state, a range over
// the jobs state index joined to the task row by key, the states' ranges
// merged by id. Without a task-column filter each range is limited before the
// join, so a page costs its own rows however many tasks finished. A row whose
// columns suffice costs no snapshot read; only a reason stored by reference,
// or a row the columns cannot reproduce, costs a blob read. Reads of finished
// tasks the drive did not materialise (Board, status task rows and the
// daemon's finished-task summary) page through it.
func (s *Store) TerminalRowsAfter(ctx context.Context, after string, limit int, f TerminalFilter) ([]TerminalRow, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("terminal page limit must be 1..1000")
	}
	if f.MinPriority < 0 {
		return nil, errors.New("terminal page minimum priority must be nonnegative")
	}
	states := f.States
	if len(states) == 0 {
		states = []string{"Cancelled", "Done", "Retired"}
	}
	// Job-column predicates, unqualified and on the joined job row.
	jobs, joined, jobArgs := ``, ``, []any{}
	for _, c := range [][2]string{{"target_id", f.Target}, {"workflow_id", f.Workflow}, {"molecule_id", f.Molecule}} {
		if c[1] != "" {
			jobs, joined = jobs+` AND `+c[0]+`=?`, joined+` AND j.`+c[0]+`=?`
			jobArgs = append(jobArgs, c[1])
		}
	}
	// Task-column predicates apply to the rows whose columns are exact.
	tasks, taskArgs := ``, []any{}
	projected := func(predicate string, arg any) {
		tasks += ` AND (t.execution_kind IS NULL OR t.mode IS NULL OR ` + predicate + `)`
		taskArgs = append(taskArgs, arg)
	}
	if f.RunID != "" {
		projected(`t.run_id=?`, f.RunID)
	}
	// LOWER folds case like Go only for ASCII; the caller alone matches any
	// other mode.
	if f.Mode != "" && ascii(f.Mode) {
		projected(`LOWER(t.mode)=LOWER(?)`, f.Mode)
	}
	if f.MinPriority > 0 {
		projected(`t.priority>=?`, f.MinPriority)
	}
	const columns = `j.job_id,j.version,j.workflow_id,j.workflow_revision,j.target_id,j.target_revision,j.state,j.payload_digest,j.payload_size,j.molecule_id,t.execution_kind,t.mode,t.run_id,t.priority,t.escalation_kind,t.escalation_reason`
	branches, args, seen := []string{}, []any{}, map[string]bool{}
	for _, state := range states {
		if state != "Cancelled" && state != "Done" && state != "Retired" {
			return nil, errors.New("terminal page states must be terminal")
		}
		if seen[state] {
			continue
		}
		seen[state] = true
		if tasks == `` {
			branches = append(branches, `(SELECT `+columns+` FROM (SELECT `+jobColumns+` FROM jobs WHERE state=? AND job_id>?`+jobs+` ORDER BY job_id LIMIT ?) j LEFT JOIN tasks t ON t.task_id=j.job_id)`)
		} else {
			branches = append(branches, `(SELECT `+columns+` FROM jobs j LEFT JOIN tasks t ON t.task_id=j.job_id WHERE j.state=? AND j.job_id>?`+joined+tasks+` ORDER BY j.job_id LIMIT ?)`)
		}
		args = append(append(append(append(args, state, after), jobArgs...), taskArgs...), limit)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM (`+strings.Join(branches, ` UNION ALL `)+`) page ORDER BY job_id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	out, refs := []TerminalRow{}, []BlobRef{}
	for rows.Next() {
		var r TerminalRow
		var ref BlobRef
		var kind, mode, run, escalation, reason sql.NullString
		var priority sql.NullInt64
		if err = rows.Scan(&r.ID, &r.Version, &r.WorkflowID, &r.WorkflowRevision, &r.TargetID, &r.TargetRevision, &r.State, &ref.Digest, &ref.Size, &r.MoleculeID, &kind, &mode, &run, &priority, &escalation, &reason); err != nil {
			rows.Close()
			return nil, err
		}
		if r.Columns = kind.Valid && mode.Valid; r.Columns {
			r.ExecutionKind, r.Mode, r.RunID, r.Priority, r.Escalation, r.Reason = kind.String, mode.String, run.String, int(priority.Int64), escalation.String, reason.String
		}
		out, refs = append(out, r), append(refs, ref)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i := range out {
		if !out[i].Columns {
			if out[i].Payload, err = s.GetBlob(ctx, refs[i]); err != nil {
				return nil, err
			}
		} else if _, ok := blobRef(out[i].Reason); ok {
			raw, err := s.ResolveText(ctx, out[i].Reason)
			if err != nil {
				return nil, err
			}
			out[i].Reason = string(raw)
		}
	}
	return out, nil
}

func ascii(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// BoardColumns are a checkpoint's task-row execution_kind and mode values:
// the job's execution kind and the admission's mode, or NULL when the task
// row cannot reproduce the Board row exactly (an admission priority below the
// column's floor of 10, or a run, mode or kind longer than its column). A
// NULL makes readers decode the snapshot instead.
func BoardColumns(kind, mode, run string, priority int) (any, any) {
	if priority < 10 || len(kind) > 255 || len(mode) > 255 || len(run) > 255 {
		return nil, nil
	}
	return kind, mode
}

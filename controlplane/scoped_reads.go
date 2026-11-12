package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Scoped reads (FAE-CP-009): the drive and the daemon's startup read only the
// rows they act on, by key or index, never every job blob or every backlog row.

// terminalStates are the job states the drive never dispatches again.
const terminalStates = `('Done','Retired','Cancelled')`

// ListActiveJobs reads every job the drive may still act on: the non-terminal
// ones, with their snapshots. Terminal jobs are read by key when needed.
func (s *Store) ListActiveJobs(ctx context.Context, limit int) ([]JobSnapshot, error) {
	return s.listJobsWhere(ctx, limit, `state NOT IN `+terminalStates)
}

// ListJobsInState reads the jobs in one state through the state index.
func (s *Store) ListJobsInState(ctx context.Context, state string, limit int) ([]JobSnapshot, error) {
	if state == "" {
		return nil, errors.New("state required")
	}
	return s.listJobsWhere(ctx, limit, `state=?`, state)
}

// FinishedTaskRow is one finished task's job row with the task-row columns
// finished-task cleanup reads before deciding whether it needs the snapshot:
// the admission's run and the execution kind (schema 11; "" on a task row
// that predates them), and whether an event of the kind the caller named
// (its own "cleaned" marker) is already recorded on the task. Payload is
// never read.
type FinishedTaskRow struct {
	JobSnapshot
	RunID, ExecutionKind string
	Marked               bool
}

// FinishedTasksPage is the most rows one FinishedTasksAfter read returns.
const FinishedTasksPage = 1000

// FinishedTasksAfter pages one terminal state's jobs after a job id, in id
// order, through the jobs state index, each joined by key to its task row
// and to the presence of one event kind on it; no snapshot is read, however
// many tasks have finished (FAE-CP-009). A caller pages until a page comes
// back short.
func (s *Store) FinishedTasksAfter(ctx context.Context, state, after, markerKind string, limit int) ([]FinishedTaskRow, error) {
	if state == "" || markerKind == "" || limit < 1 || limit > FinishedTasksPage {
		return nil, errors.New("state, marker kind and limit 1..1000 required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT j.job_id,j.version,j.workflow_id,j.workflow_revision,j.target_id,j.target_revision,j.state,j.payload_digest,j.payload_size,j.molecule_id,COALESCE(t.run_id,''),COALESCE(t.execution_kind,''),EXISTS (SELECT 1 FROM events e WHERE e.job_id=j.job_id AND e.kind=?) FROM jobs j LEFT JOIN tasks t ON t.task_id=j.job_id WHERE j.state=? AND j.job_id>? ORDER BY j.job_id LIMIT ?`, markerKind, state, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FinishedTaskRow{}
	for rows.Next() {
		var r FinishedTaskRow
		var ref BlobRef
		if err = rows.Scan(&r.ID, &r.Version, &r.WorkflowID, &r.WorkflowRevision, &r.TargetID, &r.TargetRevision, &r.State, &ref.Digest, &ref.Size, &r.MoleculeID, &r.RunID, &r.ExecutionKind, &r.Marked); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetJobs reads the named jobs by key; absent identities are omitted.
func (s *Store) GetJobs(ctx context.Context, ids []string) ([]JobSnapshot, error) {
	if len(ids) == 0 {
		return []JobSnapshot{}, nil
	}
	if len(ids) > 10000 {
		return nil, errors.New("at most 10000 jobs per keyed read")
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return s.listJobsWhere(ctx, len(ids), `job_id IN (?`+strings.Repeat(`,?`, len(ids)-1)+`)`, args...)
}

func (s *Store) listJobsWhere(ctx context.Context, limit int, where string, args ...any) ([]JobSnapshot, error) {
	if limit < 1 || limit > 100000 {
		return nil, errors.New("job limit must be 1..100000")
	}
	rows, e := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE `+where+` ORDER BY job_id LIMIT ?`, append(args, limit+1)...)
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
	if e = errors.Join(rows.Err(), rows.Close()); e != nil {
		return nil, e
	}
	if len(jobs) > limit {
		return nil, errors.New("job listing exceeds limit; refusing incomplete snapshot")
	}
	for i := range jobs {
		if jobs[i].Payload, e = s.GetBlob(ctx, refs[i]); e != nil {
			return nil, e
		}
	}
	return jobs, nil
}

// TerminalCounts counts the terminal jobs by state through the state index.
func (s *Store) TerminalCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state,COUNT(*) FROM jobs WHERE state IN `+terminalStates+` GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err = rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}

// JobHead is a job row without its snapshot, with the execution kind of its
// latest provenance ("" when it has none).
type JobHead struct {
	JobSnapshot
	ExecutionKind string
}

// GetJobHead reads one job's row and latest provenance kind by key.
func (s *Store) GetJobHead(ctx context.Context, id string) (JobHead, error) {
	var h JobHead
	var err error
	if h.JobSnapshot, _, err = scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE job_id=?`, id)); err != nil {
		return h, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT execution_kind FROM provenance WHERE task_id=? ORDER BY produced_at DESC LIMIT 1`, id).Scan(&h.ExecutionKind)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return h, err
}

// DependencyEdge is a backlog edge into a non-terminal dependent, with the
// dependent's state and its prerequisite's projected head.
type DependencyEdge struct {
	BacklogEdge
	DependentState string
	// Prerequisite is the prerequisite task row; Present is false when the
	// plane has no task for it.
	Prerequisite TaskHead
	Present      bool
	// Version is the prerequisite job's version (0 when absent).
	Version int64
}

// ActiveGraph is the part of the backlog the drive acts on: the non-terminal
// items and the edges into non-terminal dependents.
type ActiveGraph struct {
	Items []BacklogItem
	Edges []DependencyEdge
}

const dependencyQuery = `SELECT e.from_id,e.to_id,e.kind,d.state,COALESCE(p.state,''),COALESCE(t.escalation_reason,''),p.job_id IS NOT NULL,COALESCE(p.version,0) FROM backlog_edges e JOIN jobs d ON d.job_id=e.to_id LEFT JOIN jobs p ON p.job_id=e.from_id LEFT JOIN tasks t ON t.task_id=e.from_id WHERE `

// ActiveBacklog reads the non-terminal items and the edges into non-terminal
// dependents, each with its prerequisite's state: two indexed statements.
func (s *Store) ActiveBacklog(ctx context.Context) (ActiveGraph, error) {
	var g ActiveGraph
	rows, err := s.db.QueryContext(ctx, `SELECT item_id,COALESCE(source_ref,''),priority,acceptance_digest,COALESCE(write_class,''),state,created_at,deferred_until FROM backlog_items WHERE state NOT IN `+terminalStates+` ORDER BY item_id LIMIT 100001`)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		var i BacklogItem
		var deferred sql.NullTime
		if err = rows.Scan(&i.ID, &i.Source, &i.Priority, &i.AcceptanceDigest, &i.WriteClass, &i.State, &i.Admitted, &deferred); err != nil {
			rows.Close()
			return g, err
		}
		if i.Deferred = deferred.Valid; deferred.Valid && !deferred.Time.Equal(indefinitely) {
			i.DeferredUntil = deferred.Time.UTC()
		}
		g.Items = append(g.Items, i)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return g, err
	}
	if len(g.Items) > 100000 {
		return g, errors.New("backlog item bound exceeded")
	}
	g.Edges, err = s.dependencies(ctx, `d.state NOT IN `+terminalStates+` ORDER BY e.from_id,e.to_id,e.kind LIMIT 100001`)
	return g, err
}

// TaskDependencies reads the edges into one dependent by key.
func (s *Store) TaskDependencies(ctx context.Context, id string) ([]DependencyEdge, error) {
	return s.dependencies(ctx, `e.to_id=? ORDER BY e.from_id,e.kind LIMIT 100001`, id)
}

func (s *Store) dependencies(ctx context.Context, where string, args ...any) ([]DependencyEdge, error) {
	rows, err := s.db.QueryContext(ctx, dependencyQuery+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DependencyEdge{}
	for rows.Next() {
		var e DependencyEdge
		if err = rows.Scan(&e.From, &e.To, &e.Kind, &e.DependentState, &e.Prerequisite.State, &e.Prerequisite.Reason, &e.Present, &e.Version); err != nil {
			return nil, err
		}
		if out = append(out, e); len(out) > 100000 {
			return nil, errors.New("backlog edge bound exceeded")
		}
	}
	return out, rows.Err()
}

// ExistingJobs reports which of the named identities have a job, by key.
func (s *Store) ExistingJobs(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for start := 0; start < len(ids); start += 1000 {
		batch := ids[start:min(start+1000, len(ids))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT job_id FROM jobs WHERE job_id IN (?`+strings.Repeat(`,?`, len(batch)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RunTasks reads one run's task identities through the run index.
func (s *Store) RunTasks(ctx context.Context, run string, limit int) ([]string, error) {
	if run == "" || limit < 1 || limit > 100000 {
		return nil, errors.New("run and limit 1..100000 required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT task_id FROM tasks WHERE run_id=? ORDER BY task_id LIMIT ?`, run, limit)
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

// PendingTicketTasks names the terminal tasks whose ticket lifecycle is not
// finished: a queued emission not yet emitted, or no emitted row for the
// task's terminal step. The drive keeps them materialised for the outbox.
func (s *Store) PendingTicketTasks(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > 100000 {
		return nil, errors.New("limit must be 1..100000")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT o.task_id FROM ticket_outbox o JOIN jobs j ON j.job_id=o.task_id WHERE j.state IN `+terminalStates+` AND (NOT o.emitted OR NOT EXISTS (SELECT 1 FROM ticket_outbox x WHERE x.task_id=o.task_id AND x.desired_step=j.state AND x.emitted)) ORDER BY o.task_id LIMIT ?`, limit+1)
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
	if len(out) > limit {
		return nil, errors.New("pending ticket bound exceeded")
	}
	return out, rows.Err()
}

// BacklogMismatches names the backlog items whose state differs from their
// job's, compared in the plane rather than by reading both tables out.
func (s *Store) BacklogMismatches(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > 100000 {
		return nil, errors.New("limit must be 1..100000")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.item_id FROM backlog_items i JOIN jobs j ON j.job_id=i.item_id WHERE i.state<>j.state ORDER BY i.item_id LIMIT ?`, limit)
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

package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/engine"
	"time"
)

func (s *Store) Sequence(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT sequence FROM event_clock WHERE id=1`).Scan(&n)
	return n, err
}
func (s *Store) TaskTimes(ctx context.Context, id string) (time.Time, time.Time, error) {
	var created, updated time.Time
	err := s.db.QueryRowContext(ctx, `SELECT t.created_at,COALESCE((SELECT MIN(x.created_at) FROM transitions x WHERE x.task_id=t.task_id AND x.to_state IN ('Done','Cancelled','Retired')),t.updated_at) FROM tasks t WHERE t.task_id=?`, id).Scan(&created, &updated)
	return created.UTC(), updated.UTC(), err
}

// TreeJobs reads only one run tree from the transactionally maintained index.
func (s *Store) TreeJobs(ctx context.Context, task string) ([]JobSnapshot, error) {
	var root string
	if err := s.db.QueryRowContext(ctx, `SELECT tree_root FROM task_tree WHERE task_id=?`, task).Scan(&root); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT j.job_id,j.version,j.workflow_id,j.workflow_revision,j.target_id,j.target_revision,j.state,j.payload_digest,j.payload_size,j.molecule_id FROM task_tree t JOIN jobs j ON j.job_id=t.task_id WHERE t.tree_root=? ORDER BY t.task_id LIMIT 100001`, root)
	if err != nil {
		return nil, err
	}
	var jobs []JobSnapshot
	var refs []BlobRef
	for rows.Next() {
		j, ref, e := scanJob(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		jobs = append(jobs, j)
		refs = append(refs, ref)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	if len(jobs) > 100000 {
		return nil, errors.New("tree task limit exceeded")
	}
	for i := range jobs {
		jobs[i].Payload, err = s.GetBlob(ctx, refs[i])
		if err != nil {
			return nil, err
		}
	}
	return jobs, nil
}
func (s *Store) writeTreeIndex(ctx context.Context, tx *sql.Tx, j JobSnapshot, job engine.Job) error {
	parent := ""
	if job.Parent != nil {
		parent = job.Parent.Task
	}
	_, err := tx.ExecContext(ctx, `REPLACE INTO task_tree (task_id,tree_root,parent_task,depth,detached) VALUES (?,?,?,?,?)`, j.ID, engine.RootTask(job), parent, engine.Depth(job), job.Detached)
	return err
}
func (s *Store) backfillTreeIndex(ctx context.Context, tx *sql.Tx) error {
	for cursor := ""; ; {
		rows, err := tx.QueryContext(ctx, `SELECT job_id,payload_digest,payload_size FROM jobs WHERE job_id>? ORDER BY job_id LIMIT 500`, cursor)
		if err != nil {
			return err
		}
		type pending struct {
			id  string
			ref BlobRef
		}
		page := []pending{}
		for rows.Next() {
			var p pending
			if err = rows.Scan(&p.id, &p.ref.Digest, &p.ref.Size); err != nil {
				rows.Close()
				return err
			}
			page = append(page, p)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, p := range page {
			raw, err := getBlob(ctx, tx, p.ref)
			if err != nil {
				return err
			}
			var e struct {
				Job engine.Job `json:"job"`
			}
			if err = json.Unmarshal(raw, &e); err != nil {
				return err
			}
			if e.Job.TaskID == "" {
				e.Job.TaskID = p.id
			}
			if err = s.writeTreeIndex(ctx, tx, JobSnapshot{ID: p.id}, e.Job); err != nil {
				return err
			}
			cursor = p.id
		}
	}
}

type RunTiming struct {
	Dispatch          string
	Started, Finished time.Time
}

func (s *Store) RunTimings(ctx context.Context, task string) ([]RunTiming, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT dispatch_id,MIN(started_at),MAX(finished_at) FROM engine_runs WHERE task_id=? GROUP BY dispatch_id LIMIT 100001`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunTiming{}
	for rows.Next() {
		var r RunTiming
		if err = rows.Scan(&r.Dispatch, &r.Started, &r.Finished); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if len(out) > 100000 {
		return nil, errors.New("task run timing bound exceeded")
	}
	return out, rows.Err()
}

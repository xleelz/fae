package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
)

// The keyed reads of one task the live task verbs make (daemon/TASKS.md):
// each selects one task's rows by key or an indexed prefix, never a whole
// table (FAE-CP-009).

// BacklogItem reads one task's backlog row by key. found is false for a task
// admitted without one (a `run` admission).
func (s *Store) BacklogItem(ctx context.Context, id string) (BacklogItem, bool, error) {
	var i BacklogItem
	var deferred sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT item_id,COALESCE(source_ref,''),priority,acceptance_digest,COALESCE(write_class,''),state,created_at,deferred_until FROM backlog_items WHERE item_id=?`, id).Scan(&i.ID, &i.Source, &i.Priority, &i.AcceptanceDigest, &i.WriteClass, &i.State, &i.Admitted, &deferred)
	if errors.Is(err, sql.ErrNoRows) {
		return BacklogItem{}, false, nil
	}
	if err != nil {
		return BacklogItem{}, false, err
	}
	i.Admitted = i.Admitted.UTC()
	if i.Deferred = deferred.Valid; deferred.Valid && !deferred.Time.Equal(indefinitely) {
		i.DeferredUntil = deferred.Time.UTC()
	}
	return i, true, nil
}

// TaskDependents reads the edges out of one prerequisite by key (the edge
// table's primary key prefix), each with its dependent's state. A dependent
// the plane has no task for is left out, as TaskDependencies leaves out
// edges into one.
func (s *Store) TaskDependents(ctx context.Context, id string) ([]DependencyEdge, error) {
	return s.dependencies(ctx, `e.from_id=? ORDER BY e.to_id,e.kind LIMIT 100001`, id)
}

// TaskEventsOfKinds reads one task's newest events of the named kinds, at
// most limit, through the events_job index; they are returned oldest first.
func (s *Store) TaskEventsOfKinds(ctx context.Context, id string, kinds []string, limit int) ([]Event, error) {
	if id == "" || len(kinds) == 0 || len(kinds) > 16 || limit < 1 || limit > 10000 {
		return nil, errors.New("task, 1..16 kinds and limit 1..10000 required")
	}
	args := []any{id}
	for _, k := range kinds {
		args = append(args, k)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,kind,job_id,payload,created_at FROM events WHERE job_id=? AND kind IN (?`+strings.Repeat(",?", len(kinds)-1)+`) ORDER BY sequence DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var ev Event
		if err = rows.Scan(&ev.Sequence, &ev.Kind, &ev.JobID, &ev.Payload, &ev.Timestamp); err != nil {
			return nil, err
		}
		ev.Timestamp = ev.Timestamp.UTC()
		out = append(out, ev)
	}
	slices.Reverse(out)
	if err:=errors.Join(rows.Err(),rows.Close());err!=nil{return nil,err}
	return s.restoreEventMetadata(ctx,out)
}

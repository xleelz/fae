package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type BacklogEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}
type BacklogItem struct {
	ID               string `json:"id"`
	Source           string `json:"source_ref"`
	Priority         int    `json:"priority"`
	AcceptanceDigest string `json:"acceptance_digest"`
	WriteClass       string `json:"write_class"`
	State            string `json:"state"`
}
type BacklogGraph struct {
	Items []BacklogItem `json:"items"`
	Edges []BacklogEdge `json:"edges"`
}
type BacklogAdmission struct {
	Item BacklogItem
	Job  JobSnapshot
}

func (s *Store) Backlog(ctx context.Context) (g BacklogGraph, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT item_id,COALESCE(source_ref,''),priority,acceptance_digest,COALESCE(write_class,''),state FROM backlog_items ORDER BY item_id LIMIT 100001`)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		var i BacklogItem
		if err = rows.Scan(&i.ID, &i.Source, &i.Priority, &i.AcceptanceDigest, &i.WriteClass, &i.State); err != nil {
			rows.Close()
			return g, err
		}
		g.Items = append(g.Items, i)
		if len(g.Items) > 100000 {
			rows.Close()
			return g, errors.New("backlog item bound exceeded")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return g, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT from_id,to_id,kind FROM backlog_edges ORDER BY from_id,to_id,kind LIMIT 100001`)
	if err != nil {
		return g, err
	}
	defer rows.Close()
	for rows.Next() {
		var e BacklogEdge
		if err = rows.Scan(&e.From, &e.To, &e.Kind); err != nil {
			return g, err
		}
		g.Edges = append(g.Edges, e)
		if len(g.Edges) > 100000 {
			return g, errors.New("backlog edge bound exceeded")
		}
	}
	return g, rows.Err()
}

// ValidateBacklogEdges validates the combined graph and returns an explicit cycle path.
func ValidateBacklogEdges(ids map[string]bool, edges []BacklogEdge) error {
	adj := map[string][]string{}
	seen := map[BacklogEdge]bool{}
	for _, e := range edges {
		if e.From == e.To || !ids[e.From] || !ids[e.To] || (e.Kind != "blocking" && e.Kind != "relates") {
			return fmt.Errorf("invalid edge %s -> %s (%s)", e.From, e.To, e.Kind)
		}
		if seen[e] {
			continue
		}
		seen[e] = true
		if e.Kind == "blocking" {
			adj[e.From] = append(adj[e.From], e.To)
		}
	}
	color := map[string]int{}
	path := []string{}
	var visit func(string) error
	visit = func(id string) error {
		if color[id] == 2 {
			return nil
		}
		if color[id] == 1 {
			return fmt.Errorf("blocking cycle: %s", strings.Join(append(path, id), " -> "))
		}
		color[id] = 1
		path = append(path, id)
		for _, to := range adj[id] {
			if err := visit(to); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		color[id] = 2
		return nil
	}
	for id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

// AdmitBacklog commits edges before any item becomes visible, with all job CASs,
// content blobs, mirrors, events and authorization consumption in one transaction.
func (s *Store) AdmitBacklog(ctx context.Context, a Audit, items []BacklogAdmission, edges []BacklogEdge) ([]JobSnapshot, error) {
	out := make([]JobSnapshot, len(items))
	for i, x := range items {
		if err := validateJob(x.Job); err != nil {
			return nil, err
		}
		out[i] = x.Job
		out[i].Version++
	}
	err := s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockEvents(ctx, tx); err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, e := range edges {
			if _, err := tx.ExecContext(ctx, `INSERT INTO backlog_edges(from_id,to_id,kind,created_by,created_at) VALUES(?,?,?,?,?) ON DUPLICATE KEY UPDATE kind=VALUES(kind)`, e.From, e.To, e.Kind, a.Actor, now); err != nil {
				return err
			}
		}
		for i, x := range items {
			j := out[i]
			if x.Job.Version == 0 {
				if err := checkJobAdapterTx(ctx, tx, j); err != nil {
					return err
				}
			}
			payload, err := CanonicalJSON(j.Payload)
			if err != nil {
				return err
			}
			if err = s.putBlob(ctx, tx, payload); err != nil {
				return err
			}
			if x.Job.Version == 0 {
				_, err = tx.ExecContext(ctx, `INSERT INTO jobs (`+jobColumns+`) VALUES (?,?,?,?,?,?,?,?,?)`, j.ID, j.Version, j.WorkflowID, j.WorkflowRevision, j.TargetID, j.TargetRevision, j.State, Digest(payload), len(payload))
			} else {
				var result sql.Result
				result, err = tx.ExecContext(ctx, `UPDATE jobs SET version=?,payload_digest=?,payload_size=? WHERE job_id=? AND version=? AND state=?`, j.Version, Digest(payload), len(payload), j.ID, x.Job.Version, x.Job.State)
				if err == nil {
					var n int64
					n, err = result.RowsAffected()
					if err == nil && n != 1 {
						err = ErrConflict
					}
				}
			}
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO backlog_items(item_id,source_ref,priority,acceptance_digest,write_class,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE source_ref=VALUES(source_ref),priority=VALUES(priority),acceptance_digest=VALUES(acceptance_digest),write_class=VALUES(write_class),state=VALUES(state),updated_at=VALUES(updated_at)`, j.ID, x.Item.Source, x.Item.Priority, x.Item.AcceptanceDigest, x.Item.WriteClass, j.State, now, now)
			if err != nil {
				return err
			}
			if err = s.appendEvents(ctx, tx, j.ID, nil, "backlog_admitted"); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}
func (s *Store) EditBacklogEdge(ctx context.Context, a Audit, e BacklogEdge, remove bool) error {
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockEvents(ctx, tx); err != nil {
			return err
		}
		if remove {
			r, err := tx.ExecContext(ctx, `DELETE FROM backlog_edges WHERE from_id=? AND to_id=? AND kind=?`, e.From, e.To, e.Kind)
			if err != nil {
				return err
			}
			n, err := r.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return errors.New("edge absent")
			}
		} else {
			if _, err := tx.ExecContext(ctx, `INSERT INTO backlog_edges(from_id,to_id,kind,created_by,created_at) VALUES(?,?,?,?,?)`, e.From, e.To, e.Kind, a.Actor, time.Now().UTC()); err != nil {
				return err
			}
		}
		return s.appendEvents(ctx, tx, e.To, nil, "backlog_edge_changed")
	})
}

func (s *Store) ReconcileBacklogDone(ctx context.Context, a Audit, id, sha string) error {
	if strings.TrimSpace(sha) == "" {
		return errors.New("landing SHA required")
	}
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockEvents(ctx, tx); err != nil {
			return err
		}
		r, err := tx.ExecContext(ctx, `UPDATE backlog_items SET state='Done',updated_at=? WHERE item_id=? AND state<>'Done' AND EXISTS(SELECT 1 FROM jobs WHERE job_id=? AND state='Done')`, time.Now().UTC(), id, id)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrConflict
		}
		return s.appendEvents(ctx, tx, id, nil, "backlog_reconciled")
	})
}

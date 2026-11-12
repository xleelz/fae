package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	// Admitted is the admission time; it orders equal priorities (FAE-SCH-003).
	Admitted time.Time `json:"admitted_at,omitempty"`
	// DeferredUntil mirrors the task's deferral; zero means indefinitely.
	Deferred      bool      `json:"-"`
	DeferredUntil time.Time `json:"-"`
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
	rows, err := s.db.QueryContext(ctx, `SELECT item_id,COALESCE(source_ref,''),priority,acceptance_digest,COALESCE(write_class,''),state,created_at,deferred_until FROM backlog_items ORDER BY item_id LIMIT 100001`)
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

// DependentEdges reads the edges into one dependent by key, through the
// backlog_dependents index (FAE-CP-009): all an edge edit's readiness effect
// needs of the graph.
func (s *Store) DependentEdges(ctx context.Context, to string) ([]BacklogEdge, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT from_id,to_id,kind FROM backlog_edges WHERE to_id=? ORDER BY from_id,kind LIMIT 100001`, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BacklogEdge{}
	for rows.Next() {
		var e BacklogEdge
		if err = rows.Scan(&e.From, &e.To, &e.Kind); err != nil {
			return nil, err
		}
		if out = append(out, e); len(out) > 100000 {
			return nil, errors.New("backlog edge bound exceeded")
		}
	}
	return out, rows.Err()
}

// BlockingWalk is what BlockingPath found and read: Path runs from its start
// to its end along blocking edges (empty when there is no such chain);
// Statements and Rows count the edge reads it made.
type BlockingWalk struct {
	Path       []string
	Statements int
	Rows       int
}

// blockingWalkBound bounds the tasks and edge rows one walk reads, as
// Backlog bounds the whole graph.
const blockingWalkBound = 100000

// BlockingPath finds a chain of blocking edges from one task to another
// (FAE-OPS-014's cycle check: adding P -> D closes a cycle exactly when D
// reaches P). It reads the graph by key a level at a time from both ends,
// never the whole of it (FAE-CP-009): the dependents of the tasks reached
// from the start, by primary key, and the prerequisites of those reached back
// from the end, through backlog_dependents. Each level is read in statements
// of at most 1000 tasks, and the side that has issued fewer statements
// widens next (the end's side on a tie). The walk stops when the two sides
// meet or when either has no edge left to follow, so a task with no further
// dependents or prerequisites ends it at once, and otherwise it costs about
// twice the statements of the cheaper side. More than 100000 tasks or edge
// rows fail closed.
func (s *Store) BlockingPath(ctx context.Context, from, to string) (BlockingWalk, error) {
	var w BlockingWalk
	if from == to {
		w.Path = []string{from}
		return w, nil
	}
	// Each side maps a task it reached to its neighbour on the way back to
	// that side's end; the end itself maps to "".
	forward, backward := map[string]string{from: ""}, map[string]string{to: ""}
	ahead, behind := []string{from}, []string{to}
	spentAhead, spentBehind := 0, 0
	for len(ahead) > 0 && len(behind) > 0 {
		widenForward := spentAhead < spentBehind
		frontier, reached, other, spent := behind, backward, forward, &spentBehind
		query := `SELECT to_id,from_id FROM backlog_edges WHERE kind='blocking' AND to_id IN (?%s) ORDER BY to_id,from_id LIMIT ?`
		if widenForward {
			frontier, reached, other, spent = ahead, forward, backward, &spentAhead
			query = `SELECT from_id,to_id FROM backlog_edges WHERE kind='blocking' AND from_id IN (?%s) ORDER BY from_id,to_id LIMIT ?`
		}
		next := []string{}
		for start := 0; start < len(frontier); start += 1000 {
			batch := frontier[start:min(start+1000, len(frontier))]
			args := make([]any, 0, len(batch)+1)
			for _, id := range batch {
				args = append(args, id)
			}
			rows, err := s.db.QueryContext(ctx, fmt.Sprintf(query, strings.Repeat(`,?`, len(batch)-1)), append(args, blockingWalkBound-w.Rows+1)...)
			if err != nil {
				return w, err
			}
			w.Statements++
			*spent++
			for rows.Next() {
				var near, far string
				if err = rows.Scan(&near, &far); err != nil {
					rows.Close()
					return w, err
				}
				if w.Rows++; w.Rows > blockingWalkBound {
					rows.Close()
					return w, errors.New("backlog edge bound exceeded")
				}
				if _, seen := reached[far]; seen {
					continue
				}
				reached[far] = near
				if _, met := other[far]; met {
					rows.Close()
					w.Path = blockingChain(forward, backward, far)
					return w, nil
				}
				next = append(next, far)
			}
			if err = errors.Join(rows.Err(), rows.Close()); err != nil {
				return w, err
			}
		}
		if len(forward)+len(backward) > blockingWalkBound {
			return w, errors.New("backlog task bound exceeded")
		}
		if widenForward {
			ahead = next
		} else {
			behind = next
		}
	}
	return w, nil
}

// blockingChain joins the two sides of a walk where they met: the start's
// side back from meet, reversed, then the end's side on from meet.
func blockingChain(forward, backward map[string]string, meet string) []string {
	path := []string{}
	for id := meet; id != ""; id = forward[id] {
		path = append(path, id)
	}
	slices.Reverse(path)
	for id := backward[meet]; id != ""; id = backward[id] {
		path = append(path, id)
	}
	return path
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
		batch := map[string]bool{}
		for _, x := range items {
			batch[x.Job.ID] = true
		}
		for _, e := range edges {
			r, err := tx.ExecContext(ctx, `INSERT INTO backlog_edges(from_id,to_id,kind,created_by,created_at) VALUES(?,?,?,?,?) ON DUPLICATE KEY UPDATE kind=VALUES(kind)`, e.From, e.To, e.Kind, a.Actor, now)
			if err != nil {
				return err
			}
			// A new edge to a task already on the plane is in that task's
			// history (TaskGet's edits), as an edge edit is.
			if n, err := r.RowsAffected(); err != nil || n == 0 {
				continue
			}
			for _, id := range []string{e.From, e.To} {
				if !batch[id] {
					if err = s.appendEvents(ctx, tx, id, []Event{edgeChangedEvent(e, false)}, ""); err != nil {
						return err
					}
				}
			}
		}
		for i, x := range items {
			j := &out[i]
			var err error
			if x.Job.Version == 0 {
				if err = checkJobAdapterTx(ctx, tx, *j); err != nil {
					return err
				}
				err = s.admitJobTx(ctx, tx, a, j)
			} else {
				err = s.upsertBacklogJobTx(ctx, tx, a, j, x.Job)
			}
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO backlog_items(item_id,source_ref,priority,acceptance_digest,write_class,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE source_ref=VALUES(source_ref),priority=VALUES(priority),acceptance_digest=VALUES(acceptance_digest),write_class=VALUES(write_class),state=VALUES(state),updated_at=VALUES(updated_at)`, j.ID, x.Item.Source, x.Item.Priority, x.Item.AcceptanceDigest, x.Item.WriteClass, j.State, now, now)
			if err != nil {
				return err
			}
			// An upsert's task-edited event, if it changed a field
			// (daemon/TASKS.md); otherwise backlog_admitted.
			if err = s.appendEvents(ctx, tx, j.ID, j.Events, "backlog_admitted"); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// upsertBacklogJobTx is the version CAS of an already admitted item's mutable
// admission fields; the task keeps its state and its molecule.
func (s *Store) upsertBacklogJobTx(ctx context.Context, tx *sql.Tx, a Audit, j *JobSnapshot, old JobSnapshot) error {
	payload, err := CanonicalJSON(j.Payload)
	if err != nil {
		return err
	}
	if err = s.putBlob(ctx, tx, payload); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT molecule_id FROM jobs WHERE job_id=? AND version=? AND state=? FOR UPDATE`, j.ID, old.Version, old.State).Scan(&j.MoleculeID); errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	} else if err != nil {
		return err
	}
	if j.MoleculeID == "" {
		if j.MoleculeID, err = s.bindMoleculeTx(ctx, tx, j.WorkflowID, j.WorkflowRevision, false); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET version=?,payload_digest=?,payload_size=?,molecule_id=? WHERE job_id=? AND version=? AND state=?`, j.Version, Digest(payload), len(payload), j.MoleculeID, j.ID, old.Version, old.State)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.Join(ErrConflict, err)
	}
	return s.recordJobTx(ctx, tx, a, *j, j.MoleculeID)
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
		// The edge edit is in both tasks' histories, naming the edge.
		if err := s.appendEvents(ctx, tx, e.To, []Event{edgeChangedEvent(e, remove)}, ""); err != nil {
			return err
		}
		return s.appendEvents(ctx, tx, e.From, []Event{edgeChangedEvent(e, remove)}, "")
	})
}

// edgeChangedEvent is the backlog_edge_changed event of an added or removed edge.
func edgeChangedEvent(e BacklogEdge, removed bool) Event {
	payload, _ := json.Marshal(map[string]any{"from": e.From, "to": e.To, "kind": e.Kind, "removed": removed})
	return Event{Kind: "backlog_edge_changed", Payload: payload}
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

// TaskHead is a task's projected state and its escalation or retirement
// reason, read from the task rows without any snapshot.
type TaskHead struct {
	State  string
	Reason string
}

// TaskHeads reads every task's state and reason: two columns per row.
func (s *Store) TaskHeads(ctx context.Context) (map[string]TaskHead, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id,state,COALESCE(escalation_reason,'') FROM tasks ORDER BY task_id LIMIT 100001`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TaskHead{}
	for rows.Next() {
		var id string
		var h TaskHead
		if err = rows.Scan(&id, &h.State, &h.Reason); err != nil {
			return nil, err
		}
		if out[id] = h; len(out) > 100000 {
			return nil, errors.New("task head bound exceeded")
		}
	}
	return out, rows.Err()
}

package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/engine"
	"time"
)

// TransferRetiredEdges replaces blocking prerequisites in the same transaction
// as the task snapshot, mirror, event and audit. The event clock serializes graph
// mutations; validation runs against the transaction's complete graph.
func (s *Store) transferRetiredEdges(ctx context.Context, tx *sql.Tx, id, successor, actor string) error {
	var err error
	successor, err = terminalSuccessor(ctx, tx, id, successor)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT from_id,to_id,kind FROM backlog_edges LIMIT 100001`)
	if err != nil {
		return err
	}
	edges, moved := []BacklogEdge{}, []BacklogEdge{}
	ids := map[string]bool{id: true, successor: true}
	for rows.Next() {
		var e BacklogEdge
		if err = rows.Scan(&e.From, &e.To, &e.Kind); err != nil {
			rows.Close()
			return err
		}
		if e.From == id && e.Kind == "blocking" {
			e.From = successor
			moved = append(moved, e)
		}
		edges = append(edges, e)
		ids[e.From] = true
		ids[e.To] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(edges) > 100000 {
		return errors.New("backlog edge bound exceeded")
	}
	if err = ValidateBacklogEdges(ids, edges); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM backlog_edges WHERE from_id=? AND kind='blocking'`, id); err != nil {
		return err
	}
	for _, e := range edges {
		if e.From == successor && e.Kind == "blocking" {
			if _, err = tx.ExecContext(ctx, `INSERT IGNORE INTO backlog_edges (from_id,to_id,kind,created_by,created_at) VALUES (?,?,?,?,?)`, e.From, e.To, e.Kind, actor, time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	// Each moved edge is in the dependent's and the successor's histories
	// (daemon/TASKS.md: TaskGet lists it among their edits), naming the
	// retired task it came from.
	for _, e := range moved {
		event := repointedEdgeEvent(e, id)
		if err = s.appendEvents(ctx, tx, e.To, []Event{event}, ""); err != nil {
			return err
		}
		if err = s.appendEvents(ctx, tx, e.From, []Event{event}, ""); err != nil {
			return err
		}
	}
	return nil
}

// repointedEdgeEvent is the backlog_edge_changed event of a blocking edge a
// supersession moved from the retired task to its successor: the edge as it
// is now, with repointed_from naming the retired prerequisite.
func repointedEdgeEvent(e BacklogEdge, retired string) Event {
	payload, _ := json.Marshal(map[string]any{"from": e.From, "to": e.To, "kind": e.Kind, "removed": false, "repointed_from": retired})
	return Event{Kind: "backlog_edge_changed", Payload: payload}
}

// Resolve supersession chains from the transaction snapshot. The caller holds
// the event clock, so another retirement cannot change a link concurrently.
func terminalSuccessor(ctx context.Context, tx *sql.Tx, original, successor string) (string, error) {
	seen := map[string]bool{original: true}
	for n := 0; n < 1000; n++ {
		if seen[successor] {
			return "", errors.New("supersession cycle")
		}
		seen[successor] = true
		var state, digest string
		var size int64
		if err := tx.QueryRowContext(ctx, `SELECT state,payload_digest,payload_size FROM jobs WHERE job_id=?`, successor).Scan(&state, &digest, &size); err != nil {
			return "", err
		}
		if state != "Retired" {
			return successor, nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT chunk_index,data FROM blob_chunks WHERE digest=? ORDER BY chunk_index`, digest)
		if err != nil {
			return "", err
		}
		var raw []byte
		index := 0
		for rows.Next() {
			var i int
			var b []byte
			if err = rows.Scan(&i, &b); err != nil {
				rows.Close()
				return "", err
			}
			if i != index {
				rows.Close()
				return "", errors.New("invalid successor blob chunks")
			}
			index++
			raw = append(raw, b...)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", err
		}
		if int64(len(raw)) != size || Digest(raw) != digest {
			return "", errors.New("successor blob integrity failure")
		}
		var e struct {
			Job engine.Job `json:"job"`
		}
		if err = json.Unmarshal(raw, &e); err != nil {
			return "", err
		}
		class, reference, err := engine.ParseRetirement(e.Job.Reason)
		if err != nil {
			return "", err
		}
		if class != "superseded-by" {
			return successor, nil
		}
		successor = reference
	}
	return "", errors.New("supersession chain exceeds 1000 tasks")
}

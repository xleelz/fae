package controlplane

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// seedBacklog plants done Done backlog items with jobs, active Pending ones,
// and a blocking edge into every active item from a Done item; each Done item
// also blocks the next one.
func seedBacklog(t testing.TB, s *Store, done, active int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	digest := strings.Repeat("a", 64)
	for start := 0; start < done+active; start += 1000 {
		items, jobs, edges := []string{}, []string{}, []string{}
		var itemArgs, jobArgs, edgeArgs []any
		for i := start; i < min(start+1000, done+active); i++ {
			id, state := fmt.Sprintf("item-%06d", i), "Done"
			if i >= done {
				state = "Pending"
			}
			items = append(items, "(?,?,?,?,?,?)")
			itemArgs = append(itemArgs, id, 10, digest, state, now, now)
			jobs = append(jobs, "(?,1,'wf',1,'target',1,?,?,2,'')")
			jobArgs = append(jobArgs, id, state, digest)
			if i > 0 {
				edges = append(edges, "(?,?,'blocking','seed',?)")
				edgeArgs = append(edgeArgs, fmt.Sprintf("item-%06d", min(i-1, done-1)), id, now)
			}
		}
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO backlog_items (item_id,priority,acceptance_digest,state,created_at,updated_at) VALUES ` + strings.Join(items, ","), itemArgs},
			{`INSERT INTO jobs (` + jobColumns + `) VALUES ` + strings.Join(jobs, ","), jobArgs},
			{`INSERT INTO backlog_edges (from_id,to_id,kind,created_by,created_at) VALUES ` + strings.Join(edges, ","), edgeArgs},
		} {
			if _, e := s.db.ExecContext(ctx, q.sql, q.args...); e != nil {
				t.Fatal(e)
			}
		}
	}
}

// FAE-CP-009: the drive's backlog reads return only the rows it acts on: the
// active items and the edges into them, with each prerequisite's state; a
// Pending task's release re-reads only its own edges by key.
func TestActiveBacklogScopedOverLargePlane(t *testing.T) {
	s := testPlane(t, SchemaVersion)
	ctx := context.Background()
	seedBacklog(t, s, 20000, 25)
	started := time.Now()
	g, e := s.ActiveBacklog(ctx)
	elapsed := time.Since(started)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("active backlog: %d items, %d edges in %s over 20025 items", len(g.Items), len(g.Edges), elapsed)
	if len(g.Items) != 25 || len(g.Edges) != 25 {
		t.Fatalf("active backlog read %d items and %d edges, want 25 and 25", len(g.Items), len(g.Edges))
	}
	for _, edge := range g.Edges {
		if edge.DependentState != "Pending" || !edge.Present || edge.Prerequisite.State != "Done" || edge.From != "item-019999" {
			t.Fatalf("dependency row %+v", edge)
		}
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("active backlog read took %s, budget 200ms", elapsed)
	}
	one, e := s.TaskDependencies(ctx, "item-020003")
	if e != nil || len(one) != 1 || one[0].From != "item-019999" {
		t.Fatal(one, e)
	}
	counts, e := s.TerminalCounts(ctx)
	if e != nil || counts["Done"] != 20000 {
		t.Fatal(counts, e)
	}
	mismatched, e := s.BacklogMismatches(ctx, 10)
	if e != nil || len(mismatched) != 0 {
		t.Fatal(mismatched, e)
	}
}

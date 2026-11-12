package controlplane

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

// FAE-CP-009 with FAE-OPS-014: an edge edit reads its dependent's edges by
// key and checks for a cycle by walking from the edge's two ends, never the
// whole graph. Over 20,025 items whose 20,000 Done items form one chain,
// each read takes a handful of statements and rows, however long the chain
// on the far side; a cycle through 100 links costs about one statement per
// link. Relates edges are never followed.
func TestEdgeEditReadsScopedOverLargePlane(t *testing.T) {
	s := testPlane(t, SchemaVersion)
	ctx := context.Background()
	seedBacklog(t, s, 20000, 25)
	if _, e := s.db.ExecContext(ctx, `INSERT INTO backlog_edges (from_id,to_id,kind,created_by,created_at) VALUES ('item-020003','item-000000','relates','test',?)`, time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	into, e := s.DependentEdges(ctx, "item-020003")
	if e != nil || len(into) != 1 || into[0] != (BacklogEdge{From: "item-019999", To: "item-020003", Kind: "blocking"}) {
		t.Fatal("dependent edges", into, e)
	}
	chain := []string{}
	for i := 19900; i < 20000; i++ {
		chain = append(chain, fmt.Sprintf("item-%06d", i))
	}
	for _, c := range []struct {
		name, from, to   string
		path             []string
		statements, rows int
	}{
		// Adding item-019999 -> item-020003 again: the dependent has no
		// dependents, so the walk ends after one level from each end.
		{"leaf dependent, deep prerequisite", "item-020003", "item-019999", nil, 2, 1},
		// Adding item-020003 -> item-019999 closes the one-link cycle.
		{"one-link cycle", "item-019999", "item-020003", []string{"item-019999", "item-020003"}, 1, 1},
		// Adding item-020003 -> item-019900 closes a cycle of 101 links.
		{"long cycle", "item-019900", "item-020003", append(slices.Clone(chain), "item-020003"), 101, 101},
		// Adding a new task -> item-000000 (20,000 dependents downstream):
		// the new prerequisite has no prerequisites of its own.
		{"fresh prerequisite, deep dependent", "item-000000", "fresh", nil, 1, 0},
		// item-020003 relates to item-000000 but does not block it.
		{"relates not followed", "item-020003", "item-000000", nil, 1, 0},
	} {
		started := time.Now()
		w, e := s.BlockingPath(ctx, c.from, c.to)
		elapsed := time.Since(started)
		if e != nil {
			t.Fatal(c.name, e)
		}
		t.Logf("%s: %d statements, %d rows in %s", c.name, w.Statements, w.Rows, elapsed)
		if !slices.Equal(w.Path, c.path) {
			t.Fatalf("%s: path %v, want %v", c.name, w.Path, c.path)
		}
		if w.Statements > c.statements || w.Rows > c.rows {
			t.Fatalf("%s: walk unscoped: %d statements and %d rows, at most %d and %d", c.name, w.Statements, w.Rows, c.statements, c.rows)
		}
	}
	// A level wider than 1000 tasks is read in batches: item-019999's 25
	// dependents plus 1,500 more, none with a dependent of its own. The
	// 1,525 leaves end the walk in two statements; the chain on the other
	// side is read only as far as the statements spent on them.
	now := time.Now().UTC()
	for start := 0; start < 1500; start += 500 {
		values, args := "", []any{}
		for i := start; i < start+500; i++ {
			if values != "" {
				values += ","
			}
			values += "(?,?,'blocking','test',?)"
			args = append(args, "item-019999", fmt.Sprintf("wide-%04d", i), now)
		}
		if _, e = s.db.ExecContext(ctx, `INSERT INTO backlog_edges (from_id,to_id,kind,created_by,created_at) VALUES `+values, args...); e != nil {
			t.Fatal(e)
		}
	}
	w, e := s.BlockingPath(ctx, "item-019999", "item-019000")
	if e != nil || len(w.Path) != 0 || w.Statements > 5 || w.Rows > 1527 {
		t.Fatalf("wide level: %+v %v", w, e)
	}
	t.Logf("wide level: %d statements, %d rows", w.Statements, w.Rows)
}

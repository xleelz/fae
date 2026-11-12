package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
)

// plantFarEdges writes n blocking edges, one chain far-000000 -> far-000001
// -> ..., unconnected to any task, in one statement generated in the plane.
func plantFarEdges(t *testing.T, d *Daemon, n int) {
	t.Helper()
	digit := `(SELECT 0 i UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9)`
	err := d.store.Mutate(context.Background(), controlplane.Audit{Actor: "test", Action: "plant backlog edges", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO backlog_edges (from_id,to_id,kind,created_by,created_at) SELECT CONCAT('far-',LPAD(k,6,'0')),CONCAT('far-',LPAD(k+1,6,'0')),'blocking','test',? FROM (SELECT d0.i+10*d1.i+100*d2.i+1000*d3.i+10000*d4.i+100000*d5.i AS k FROM `+digit+` d0,`+digit+` d1,`+digit+` d2,`+digit+` d3,`+digit+` d4,(SELECT 0 i UNION ALL SELECT 1) d5) g WHERE k<?`, time.Now().UTC(), n)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// FAE-CP-009 with FAE-OPS-014 and FAE-CLI-015: a backlog edge edit reads its
// dependent's edges by key, before the writer lock and again under it, and
// walks only between the edge's two ends, under the lock; it never reads the
// whole graph.
// On a plane holding more backlog edges than a whole-graph read accepts
// (100,000), an add and a remove still report their readiness effect, a
// blocking edge that closes a cycle through a chain of prerequisites is still
// refused naming the cycle, and the same edge as relates is accepted.
func TestBacklogEdgeEditScopedPastTheGraphBound(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	stopDrive(t, d)
	item := func(id string) BacklogItemParams {
		return BacklogItemParams{AdmitParams: AdmitParams{ID: id, Tree: "tree", Priority: 10, Content: "accept " + id}}
	}
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{item("a"), item("b"), item("c"), item("x")}, Edges: []controlplane.BacklogEdge{{From: "a", To: "b", Kind: "blocking"}, {From: "b", To: "c", Kind: "blocking"}}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	plantFarEdges(t, d, 100001)
	if _, err = d.store.Backlog(ctx); err == nil || !strings.Contains(err.Error(), "bound exceeded") {
		t.Fatal("the plane is within the whole-graph read's bound: ", err)
	}
	type effect struct {
		Effect string `json:"effect"`
		After  struct {
			Ready    bool     `json:"ready"`
			Withheld []string `json:"withheld_by"`
		} `json:"after"`
	}
	edit := func(e controlplane.BacklogEdge, remove bool) (effect, error) {
		r, err := call(t, d, "BacklogEdge", BacklogEdgeParams{BacklogEdge: e, Remove: remove, AuthorizingMessage: "message"}, "")
		var out struct {
			Readiness effect `json:"readiness_effect"`
		}
		if err == nil {
			err = json.Unmarshal(r.Result, &out)
		}
		return out.Readiness, err
	}
	closing := controlplane.BacklogEdge{From: "c", To: "a", Kind: "blocking"}
	if _, err = edit(closing, false); err == nil || !strings.Contains(err.Error(), "blocking cycle: c -> a -> b -> c") {
		t.Fatal("a cycle through a chain not refused by name: ", err)
	}
	closing.Kind = "relates"
	if _, err = edit(closing, false); err != nil {
		t.Fatal("a relates edge refused: ", err)
	}
	hold := controlplane.BacklogEdge{From: "a", To: "x", Kind: "blocking"}
	if r, err := edit(hold, false); err != nil || r.Effect != "dependent now withheld" || r.After.Ready || len(r.After.Withheld) != 1 || r.After.Withheld[0] != "a: Pending" {
		t.Fatalf("add: %+v %v", r, err)
	}
	if r, err := edit(hold, true); err != nil || r.Effect != "dependent now releasable" || !r.After.Ready {
		t.Fatalf("remove: %+v %v", r, err)
	}
	if _, err = edit(hold, true); err == nil || !strings.Contains(err.Error(), "edge absent") {
		t.Fatal("absent edge removal not refused: ", err)
	}
}

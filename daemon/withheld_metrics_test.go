package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

// FAE-SCH-010 acceptance: every round with withheld tasks writes a trace
// entry, even when the withheld set is unchanged, and both status and Metrics
// show withheld counts for every reason, not only edges.
func TestWithheldReasonsInMetricsAndTraceEveryRound(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.MaxInFlight = 1
	gated := &drainExecutor{started: make(chan string, 4), release: make(chan struct{}), delegate: engine.NewScripted(defaults.HappyScript())}
	cfg.Executor = gated
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(t, d) })
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	items := []BacklogItemParams{}
	for _, id := range []string{"a", "b", "c"} {
		items = append(items, BacklogItemParams{AdmitParams: AdmitParams{ID: id, Tree: "tree", Priority: 10, Content: "accept " + id}, Source: "ticket/" + id})
	}
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: items, Edges: []controlplane.BacklogEdge{{From: "a", To: "c", Kind: "blocking"}}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10 && len(gated.started) == 0; i++ {
		if err = d.round(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	traces := func() int {
		raw, err := os.ReadFile(d.layout.Log)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(raw), "drive withheld round=")
	}
	before := traces()
	for i := 0; i < 3; i++ {
		if err = d.round(); err != nil {
			t.Fatal(err)
		}
	}
	if got := traces(); got != before+3 {
		t.Fatalf("withheld trace entries: %d rounds wrote %d", 3, got-before)
	}
	metrics := rpcResult[map[string]json.RawMessage](t, d, "Metrics", MetricsParams{})
	var byReason map[string]int
	var totals map[string]uint64
	if err = json.Unmarshal(metrics["withheld_by_reason"], &byReason); err != nil || byReason[withheldTasksInFlight] != 1 || byReason[withheldEdge] != 1 {
		t.Fatalf("metrics withheld_by_reason %s", metrics["withheld_by_reason"])
	}
	if err = json.Unmarshal(metrics["withheld_totals"], &totals); err != nil || totals[withheldTasksInFlight] < 3 || totals[withheldEdge] < 3 {
		t.Fatalf("metrics withheld_totals %s", metrics["withheld_totals"])
	}
	var edges map[string]int
	if err = json.Unmarshal(metrics["withheld_edges_by_prerequisite"], &edges); err != nil || len(edges) != 1 {
		t.Fatalf("metrics edge detail %s", metrics["withheld_edges_by_prerequisite"])
	}
}

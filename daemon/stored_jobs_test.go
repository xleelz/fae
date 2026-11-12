package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
)

// FAE-CP-009: a restarted daemon materialises only the jobs it may still act
// on. Finished tasks are counted in status and read by key when named: show
// by task and by run, history, a replayed admission that must not resurrect
// them, and a new dependent released by a finished prerequisite.
func TestRestartMaterialisesOnlyActiveJobs(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	finished := []string{"finished-a", "finished-b"}
	for _, id := range finished {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, RunID: "lazy-run", Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range finished {
		awaitState(t, d, id, "Done")
	}
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	_, loaded := d.jobs["finished-a"]
	stored := d.storedTerminal["Done"]
	d.mu.Unlock()
	if loaded || stored != 2 {
		t.Fatalf("finished task materialised at startup (%v) or uncounted (%d)", loaded, stored)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if done, _ := lookup(statusBody(t, d, nil), "snapshot.counts.Done"); done == float64(2) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status does not count stored finished tasks")
		}
		time.Sleep(50 * time.Millisecond)
	}
	r, err := call(t, d, "Show", ShowParams{RunID: "lazy-run"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var shown struct {
		Tasks []ShowTask `json:"tasks"`
	}
	if err = json.Unmarshal(r.Result, &shown); err != nil || len(shown.Tasks) != 2 || shown.Tasks[0].Stage != "Done" {
		t.Fatalf("show run %s %v", r.Result, err)
	}
	if _, err = call(t, d, "History", HistoryParams{ID: "finished-b", Summary: true, Limit: 5}, ""); err != nil {
		t.Fatal(err)
	}
	// A replayed admission answers with the original response only.
	if _, err = call(t, d, "Admit", AdmitParams{ID: "finished-a", RunID: "lazy-run", Tree: "tree", AuthorizingMessage: "message"}, "finished-a"); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	_, loaded = d.jobs["finished-a"]
	d.mu.Unlock()
	if loaded {
		t.Fatal("a replayed admission resurrected a finished task's original checkpoint")
	}
	// A finished prerequisite is known and satisfied, never "missing".
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{{AdmitParams: AdmitParams{ID: "dependent", Tree: "tree", Priority: 10, Content: "dependent"}}}, Edges: []controlplane.BacklogEdge{{From: "finished-a", To: "dependent", Kind: "blocking"}}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "dependent", "Done")
	if _, err = call(t, d, "BacklogEdge", BacklogEdgeParams{BacklogEdge: controlplane.BacklogEdge{From: "finished-b", To: "dependent", Kind: "relates"}, AcceptInFlight: true, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal("edge from a stored finished task refused:", err)
	}
}

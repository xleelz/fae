package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

func TestGraphOfDefaultWorkflowNormalPath(t *testing.T) {
	doc := defaults.Workflow()
	g := graphOf(doc)
	if g.stages[0].Stage != "Pending" || g.stages[1].Stage != doc.Start || len(g.roles) == 0 || g.roles[0] != "spec" {
		t.Fatalf("%+v %v", g.stages[:2], g.roles)
	}
	for _, stage := range []string{"commit", "integrate", "Escalated"} {
		if _, ok := g.index[stage]; !ok {
			t.Fatal("graph misses", stage)
		}
	}
	if !g.stages[g.index["commit"]].NormalPath || g.stages[g.index["Escalated"]].NormalPath {
		t.Fatal("normal path marks")
	}
}

// FAE-WF-052, -051, FAE-CP-009: a Done task's history derives from the plane's
// transitions, artifacts and provenance alone, with every role and output; a
// chain query walks from its commit back to the spec; the topology read pages
// running molecules with the tasks at each stage.
func TestRecordsHistoryChainAndTopology(t *testing.T) {
	cfg, ctx := fixture(t)
	script := defaults.HappyScript()
	script["slow/intake"] = []engine.ScriptStep{{Output: script["intake"][0].Output, Delay: 3 * time.Second}}
	cfg.Executor = engine.NewScripted(script)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, e = call(t, d, "Admit", AdmitParams{ID: "one", Tree: "tree", AuthorizingMessage: "message"}, "one"); e != nil {
		t.Fatal(e)
	}
	awaitState(t, d, "one", "Done")
	transitions := []controlplane.TransitionRecord{}
	roles, outputs := map[string]bool{}, 0
	decisions := 0
	cursor := int64(0)
	for pages := 0; ; pages++ {
		r, e := call(t, d, "History", HistoryParams{ID: "one", Records: true, After: cursor, Limit: 7}, "")
		var page RecordsPage
		decodeResult(t, r, e, &page)
		if page.MoleculeID == "" || len(page.Transitions) > 7 || pages > 100 {
			t.Fatal("records page", string(r.Result))
		}
		transitions = append(transitions, page.Transitions...)
		decisions += len(page.Decisions)
		for _, a := range page.Artifacts {
			roles[a.Role] = true
			if len(a.Output) > 0 && a.Kind != "" {
				outputs++
			}
		}
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}
	if len(transitions) == 0 || transitions[0].From != "Pending" || transitions[len(transitions)-1].To != "Done" || decisions == 0 {
		t.Fatal("transition log", len(transitions), decisions)
	}
	for i := 1; i < len(transitions); i++ {
		if transitions[i].From != transitions[i-1].To {
			t.Fatal("transition log has a gap", transitions[i-1], transitions[i])
		}
	}
	for _, role := range []string{"spec", "spec-reviewer", "planner", "coder", "reviewer-correctness", "oracle"} {
		if !roles[role] {
			t.Fatal("history misses role", role, roles)
		}
	}
	if outputs < 20 {
		t.Fatal("history misses outputs", outputs)
	}
	r, e := call(t, d, "ProvenanceChain", ChainParams{Commit: "scripted-commit"}, "")
	var chain struct {
		Hops []controlplane.ProvenanceHop `json:"hops"`
	}
	decodeResult(t, r, e, &chain)
	stages := map[string]bool{}
	for _, hop := range chain.Hops {
		stages[hop.Stage] = true
	}
	if !stages["commit"] || !stages["spec"] || !stages["intake"] {
		t.Fatal("chain", stages)
	}
	if _, e = call(t, d, "ProvenanceChain", ChainParams{Commit: "absent"}, ""); e == nil {
		t.Fatal("unknown commit accepted")
	}
	if _, e = call(t, d, "Admit", AdmitParams{ID: "slow", Tree: "tree", AuthorizingMessage: "message"}, "slow"); e != nil {
		t.Fatal(e)
	}
	awaitState(t, d, "slow", "intake")
	r, e = call(t, d, "Topology", TopologyParams{}, "")
	var topology TopologyPage
	decodeResult(t, r, e, &topology)
	if len(topology.Molecules) != 1 || topology.Statements != 2 || topology.Molecules[0].RoleOrder[0] != "spec" {
		t.Fatal("topology", string(r.Result))
	}
	found := false
	for _, s := range topology.Molecules[0].Stages {
		if s.Stage == "intake" && len(s.Tasks) == 1 && s.Tasks[0] == "slow" {
			found = true
		}
	}
	if !found {
		t.Fatal("topology lacks the running task at its stage", string(r.Result))
	}
	raw, _ := json.Marshal(topology)
	if len(raw) > readPageBytes {
		t.Fatal("topology page bound")
	}
}

// FAE-ROB-020, FAE-API-001: Topology answers from a cached snapshot and
// reports its age. Concurrent reads of a cold cache share one plane refresh
// and a saturated read pool delays none of them; a snapshot older than 5 s is
// served while a refresh runs, and one older than 10 s is refreshed before
// the read answers.
func TestTopologyServedFromCachedSnapshot(t *testing.T) {
	cfg, ctx := fixture(t)
	script := defaults.HappyScript()
	script["slow/intake"] = []engine.ScriptStep{{Output: script["intake"][0].Output, Delay: 20 * time.Second}}
	cfg.Executor = engine.NewScripted(script)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, e = call(t, d, "Admit", AdmitParams{ID: "slow", Tree: "tree", AuthorizingMessage: "message"}, "slow"); e != nil {
		t.Fatal(e)
	}
	awaitState(t, d, "slow", "intake")
	read := func() (TopologyPage, time.Duration) {
		t.Helper()
		begin := time.Now()
		r, e := call(t, d, "Topology", TopologyParams{}, "")
		elapsed := time.Since(begin)
		var page TopologyPage
		decodeResult(t, r, e, &page)
		if !strings.Contains(string(r.Result), `"snapshot_age_ms"`) || page.SnapshotAt.IsZero() || page.Statements != 2 || len(page.Molecules) != 1 {
			t.Fatalf("topology %s", r.Result)
		}
		for _, s := range page.Molecules[0].Stages {
			if s.Stage == "intake" && len(s.Tasks) == 1 && s.Tasks[0] == "slow" {
				return page, elapsed
			}
		}
		t.Fatalf("topology lacks the running task at its stage: %s", r.Result)
		return page, elapsed
	}
	release := saturateReadPool(t, d)
	defer release()
	type result struct {
		page    TopologyPage
		elapsed time.Duration
	}
	results := make(chan result, 4)
	for range 4 {
		go func() {
			page, elapsed := read()
			results <- result{page, elapsed}
		}()
	}
	for range 4 {
		if r := <-results; r.elapsed >= 2*time.Second {
			t.Errorf("topology took %s with the read pool saturated", r.elapsed)
		}
	}
	if n := d.topologyCache.refreshes.Load(); n != 1 {
		t.Fatalf("4 concurrent topology reads ran %d refreshes", n)
	}
	age := func(by time.Duration) {
		c := &d.topologyCache
		c.mu.Lock()
		aged := *c.snapshot
		aged.at = aged.at.Add(-by)
		c.snapshot = &aged
		c.mu.Unlock()
	}
	if read(); d.topologyCache.refreshes.Load() != 1 {
		t.Fatal("a fresh snapshot was refreshed")
	}
	age(6 * time.Second)
	if page, _ := read(); page.SnapshotAgeMillis < 6000 {
		t.Fatalf("aged snapshot not served while it refreshes: age %d ms", page.SnapshotAgeMillis)
	}
	for deadline := time.Now().Add(5 * time.Second); d.topologyCache.refreshes.Load() != 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("an aged snapshot woke no refresh")
		}
	}
	age(11 * time.Second)
	if page, _ := read(); page.SnapshotAgeMillis >= 5000 || d.topologyCache.refreshes.Load() != 3 {
		t.Fatalf("stale snapshot served: age %d ms, %d refreshes", page.SnapshotAgeMillis, d.topologyCache.refreshes.Load())
	}
}

package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

// topologyMoleculeView decodes a Topology molecule by its JSON alone, so the
// tests read the same fields on a daemon that does not know `error`.
type topologyMoleculeView struct {
	WorkflowID string          `json:"workflow_id"`
	RoleOrder  []string        `json:"role_order"`
	Stages     []TopologyStage `json:"stages"`
	Error      string          `json:"error"`
}

func topologyMolecules(t *testing.T, d *Daemon, params map[string]any) ([]topologyMoleculeView, string) {
	t.Helper()
	r, err := call(t, d, "Topology", params, "")
	var page struct {
		Molecules []topologyMoleculeView `json:"molecules"`
	}
	decodeResult(t, r, err, &page)
	return page.Molecules, string(r.Result)
}

// FAE-API-001, FAE-WF-093: a running molecule whose revision this binary
// cannot run is rendered with its error and its tasks by state, and does not
// hide the other molecules. Before, one such molecule failed the whole
// Topology page (and so the TUI's Molecules view).
func TestTopologyRendersAMoleculeOnAnUnrunnableRevision(t *testing.T) {
	cfg, ctx := fixture(t)
	script := defaults.HappyScript()
	script["slow/intake"] = []engine.ScriptStep{{Output: script["intake"][0].Output, Delay: 20 * time.Second}}
	cfg.Executor = engine.NewScripted(script)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "slow", Tree: "tree", AuthorizingMessage: "message"}, "slow"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "slow", "intake")
	legacy := unrunnableLegacyDocument("legacy")
	raw, _ := json.Marshal(legacy)
	if _, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "older binary install", Outcome: "ok"}, "legacy", "legacy", raw, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	plantPinnedTask(t, d, cfg, "stuck", "legacy", 1, legacy)
	molecules, result := topologyMolecules(t, d, nil)
	if len(molecules) != 2 {
		t.Fatalf("topology: %s", result)
	}
	byWorkflow := map[string]topologyMoleculeView{}
	for _, m := range molecules {
		byWorkflow[m.WorkflowID] = m
	}
	bad, good := byWorkflow["legacy"], byWorkflow["default"]
	if !strings.Contains(bad.Error, "revision cannot be run by this binary (FAE-WF-093)") || !strings.Contains(bad.Error, "brief key must be default") || len(bad.Stages) != 1 || bad.Stages[0].Stage != "Pending" || fmt.Sprint(bad.Stages[0].Tasks) != "[stuck]" || bad.Stages[0].Primitive != "" || len(bad.RoleOrder) != 0 {
		t.Fatalf("the unrunnable molecule: %+v", bad)
	}
	if good.Error != "" || len(good.RoleOrder) == 0 {
		t.Fatalf("the valid molecule: %+v", good)
	}
	for _, s := range good.Stages {
		if s.Stage == "intake" && fmt.Sprint(s.Tasks) == "[slow]" {
			return
		}
	}
	t.Fatalf("the valid molecule lost its running task: %s", result)
}

// FAE-OPS-011, FAE-CP-009, FAE-API-001: Topology hides a running molecule's
// retired tasks by default and lists them at a "Retired" stage with
// include_retired, as Status, Board, History and Events do with their opt-in.
// The default snapshot neither reads Retired task rows nor counts them toward
// its 10,000-task bound, so a running molecule whose Retired history passes
// 10,000 tasks still serves the default Topology page.
func TestTopologyListsRetiredTasksOnlyWhenAsked(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	admitHeld(t, d, "keep", "gone")
	var deleted map[string]any
	if err = taskCall(t, d, "TaskDelete", TaskDeleteParams{ID: "gone", Reason: "not needed", AuthorizingMessage: "message"}, &deleted); err != nil || deleted["action"] != "withdrawn" {
		t.Fatal(deleted, err)
	}
	if row, err := d.store.GetJob(ctx, "gone"); err != nil || row.State != "Retired" {
		t.Fatal("the deleted task is not Retired", row.State, err)
	}
	stages := func(includeRetired bool) (map[string]string, TopologyPage) {
		t.Helper()
		r, err := call(t, d, "Topology", map[string]any{"include_retired": includeRetired}, "")
		var page TopologyPage
		decodeResult(t, r, err, &page)
		if len(page.Molecules) != 1 {
			t.Fatalf("topology: %s", r.Result)
		}
		at := map[string]string{}
		for _, s := range page.Molecules[0].Stages {
			for _, task := range s.Tasks {
				at[task] = s.Stage
			}
		}
		return at, page
	}
	at, defaultPage := stages(false)
	if at["keep"] != "Pending" || at["gone"] != "" || defaultPage.Rows != 2 {
		t.Fatalf("by default: stages=%v rows=%d (want 2: 1 molecule + 1 live task)", at, defaultPage.Rows)
	}
	at, retiredPage := stages(true)
	if at["keep"] != "Pending" || at["gone"] != "Retired" || retiredPage.Rows != 3 {
		t.Fatalf("with include_retired: stages=%v rows=%d (want 3: 1 molecule + 2 tasks)", at, retiredPage.Rows)
	}
	// Plant 10,000 more Retired tasks on the running molecule (10,002 tasks
	// in all, 1 of them non-terminal). The default view still reads only the
	// 1 live task and succeeds; only include_retired hits the 10,000 bound.
	molID := defaultPage.Molecules[0].ID
	now := time.Now().UTC()
	for start := 0; start < 10000; start += 1000 {
		values, args := []string{}, []any{}
		for i := start; i < start+1000; i++ {
			values = append(values, "(?,?,?,?,?,?,?,?,?,?,?,?)")
			args = append(args, fmt.Sprintf("retired-%05d", i), "run", molID, "target", 1, "Retired", 1, 20, 10, `{"mode":"Local"}`, now, now)
		}
		if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "plant retired history", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
			_, e := tx.ExecContext(ctx, `INSERT INTO tasks (task_id,run_id,molecule_id,target_id,target_revision,state,attempt,attempt_limit,priority,disposition,created_at,updated_at) VALUES `+strings.Join(values, ","), args...)
			return e
		}); err != nil {
			t.Fatal(err)
		}
	}
	d.topologyCache.mu.Lock()
	d.topologyCache.snapshot = nil
	d.topologyCache.mu.Unlock()
	if at, defaultPage = stages(false); at["keep"] != "Pending" || defaultPage.Rows != 2 {
		t.Fatalf("default page after 10,001 retired tasks: stages=%v rows=%d", at, defaultPage.Rows)
	}
	if _, err := d.store.Topology(ctx, "", "", 100, 10000, true); !errors.Is(err, controlplane.ErrTopologyBound) {
		t.Fatalf("include_retired accepted 10,002 tasks: %v", err)
	}
}

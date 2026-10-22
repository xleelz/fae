package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
)

func TestDynamicPaneConfigurationAuditedRestart(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	binary := filepath.Join(os.Getenv("HOME"), ".local/bin/herdr")
	if _, err := os.Stat(binary); err != nil {
		t.Skip("Herdr required")
	}
	cfg.Live.Adapter = "multiplexer"
	cfg.Live.HerdrBinary = binary
	cfg.Live.PaneLimit = 60
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	if _, err = call(t, d, "RuntimeConfig", RuntimeConfig{PaneLimit: 30, AuthorizingMessage: "missing"}, ""); err == nil {
		t.Fatal("unauthorized update")
	}
	authorize(t, d)
	if _, err = call(t, d, "RuntimeConfig", RuntimeConfig{PaneLimit: 2, AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("floor violation")
	}
	response, err := call(t, d, "RuntimeConfig", RuntimeConfig{PaneLimit: 30, AuthorizingMessage: "message"}, "")
	if err != nil {
		t.Fatal(err)
	}
	responseJSON, _ := json.Marshal(response.Result)
	var configured map[string]any
	if err = json.Unmarshal(responseJSON, &configured); err != nil || configured["pane_limit"] != float64(30) || configured["effective_pane_limit"] != float64(30) {
		t.Fatal(configured, err)
	}
	if got := d.dynamicConfiguration()["pane_limit"]; got != 30 {
		t.Fatal("status requested limit", got)
	}
	// Another live instance can impose a stricter shared limit without rewriting
	// this instance's requested setting reported by status.
	if err = d.live.guard.panes.Configure(ctx, "other-live-instance", 20); err != nil {
		t.Fatal(err)
	}
	if err = d.dispatch(); err != nil {
		t.Fatal(err)
	}
	requested := d.dynamicConfiguration()
	if requested["pane_limit"] != 30 || requested["effective_pane_limit"] != 20 {
		t.Fatal(requested)
	}
	if err = d.live.guard.panes.Unconfigure(ctx, "other-live-instance", d.live.guard.owner, d.live.guard.birth); err != nil {
		t.Fatal(err)
	}
	if err = d.dispatch(); err != nil {
		t.Fatal(err)
	}
	if n := d.live.guard.effectivePaneLimit(); n != 30 {
		t.Fatal(n)
	}
	raw, err := d.store.GetMarker(ctx, "runtime-config")
	if err != nil {
		t.Fatal(err)
	}
	var persisted RuntimeConfig
	if err = json.Unmarshal(raw, &persisted); err != nil || persisted.PaneLimit != 30 {
		t.Fatal(persisted, err)
	}
	var audits int
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect audit", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit WHERE action='RuntimeConfig' AND message_id='message'").Scan(&audits)
	})
	if err != nil || audits != 1 {
		t.Fatal(audits, err)
	}
	// Exercise the dispatch reread independently of the RPC update cache.
	err = d.store.PutMarker(ctx, controlplane.Audit{Actor: "test", Action: "external dynamic update", Outcome: "ok"}, "runtime-config", json.RawMessage(`{"pane_limit":25}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.dispatch(); err != nil {
		t.Fatal(err)
	}
	if n := d.live.guard.effectivePaneLimit(); n != 25 {
		t.Fatal("round retained old limit", n)
	}
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n := d.live.guard.effectivePaneLimit(); n != 25 {
		t.Fatal("restart lost update", n)
	}
	status, err := call(t, d, "Status", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(status.Result)
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	snapshot := out["snapshot"].(map[string]any)
	configuration := snapshot["configuration"].(map[string]any)
	if configuration["dynamic"].(map[string]any)["pane_limit"] != float64(25) || configuration["dynamic"].(map[string]any)["effective_pane_limit"] != float64(25) {
		t.Fatal(configuration)
	}
	if configuration["fixed"].(map[string]any)["socket_path"] != d.layout.Socket {
		t.Fatal(configuration)
	}
}

func TestRestartValidatesDurablePaneFloorAndReleasesRegistration(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	binary := filepath.Join(os.Getenv("HOME"), ".local/bin/herdr")
	if _, err := os.Stat(binary); err != nil {
		t.Skip("Herdr required")
	}
	cfg.Live.Adapter = "multiplexer"
	cfg.Live.HerdrBinary = binary
	cfg.Live.PaneLimit = 200
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			stop(t, d)
		}
	}()
	d.mu.Lock()
	d.draining = true
	doc := defaults.Workflow()
	doc.ID = "custom-pane-floor"
	changed := false
	for id, role := range doc.Roles {
		if role.Scope == "workflow-global" {
			role.Cap = 100
			doc.Roles[id] = role
			changed = true
			break
		}
	}
	if !changed {
		d.mu.Unlock()
		t.Fatal("fixture needs global role")
	}
	raw, _ := json.Marshal(doc)
	_, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "test", Action: "install custom", Outcome: "ok"}, doc.ID, doc.ID, raw, validWorkflow)
	if err != nil {
		d.mu.Unlock()
		t.Fatal(err)
	}
	job, err := engine.NewLive("custom-task", 1, doc, "tree")
	if err != nil {
		d.mu.Unlock()
		t.Fatal(err)
	}
	job.Parked = true
	job.Reason = "infrastructure-deferred"
	raw, _ = json.Marshal(envelope{Job: job})
	snapshot, err := d.store.AdmitJob(ctx, controlplane.Audit{Actor: "test", Action: "admit custom", Outcome: "ok"}, "custom-task", controlplane.JobSnapshot{ID: job.TaskID, WorkflowID: doc.ID, WorkflowRevision: 1, TargetID: cfg.Workspace, TargetRevision: 1, State: string(job.State), Payload: raw})
	if err != nil {
		d.mu.Unlock()
		t.Fatal(err)
	}
	d.jobs[job.TaskID] = snapshot
	ledger := d.live.guard.panes
	if err = ledger.Configure(ctx, "other-instance", 1000); err != nil {
		d.mu.Unlock()
		t.Fatal(err)
	}
	d.mu.Unlock()
	stop(t, d)
	closed = true
	if limit, err := ledger.Limit(ctx); err != nil || limit != 1000 {
		t.Fatal("Close retained registration", limit, err)
	}
	if err = ledger.Configure(ctx, "other-instance", 40); err != nil {
		t.Fatal(err)
	}
	restarted, err := Start(ctx, cfg)
	if err == nil {
		stop(t, restarted)
		t.Fatal("durable custom workflow below floor restarted")
	}
	if !strings.Contains(err.Error(), "custom-task") || !strings.Contains(err.Error(), "pane limit 40") {
		t.Fatal(err)
	}
	// The failed start ran in this still-live process; birth-based cleanup alone
	// cannot remove its registration. Raising the other instance exposes leaks.
	if err = ledger.Configure(ctx, "other-instance", 1000); err != nil {
		t.Fatal(err)
	}
	if limit, err := ledger.Limit(ctx); err != nil || limit != 1000 {
		t.Fatal("failed Start retained registration", limit, err)
	}
}

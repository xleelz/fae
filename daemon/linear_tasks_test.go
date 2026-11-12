package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xleelz/fae/symphony"
	"github.com/xleelz/fae/tracker"
)

type linearStateFixture struct {
	statuses          map[string]LinearTaskStatus
	admitted, stopped int
}

func (b *linearStateFixture) AdmitLinearIssue(_ context.Context, issue symphony.Issue, _ string, _ string, _ string, _ int) (string, error) {
	if status, ok := b.statuses[issue.ID]; ok {
		return status.TaskID, nil
	}
	b.admitted++
	id := "task-" + issue.ID
	b.statuses[issue.ID] = LinearTaskStatus{TaskID: id, Identifier: issue.Identifier, State: "implement", Version: 1}
	return id, nil
}
func (b *linearStateFixture) StopLinearTask(_ context.Context, id, _ string) error {
	b.stopped++
	for key, status := range b.statuses {
		if status.TaskID == id {
			status.State = "Retired"
			status.Terminal = true
			b.statuses[key] = status
		}
	}
	return nil
}

func TestLinearEmbeddedHotReloadReplacesTrackerScope(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "WORKFLOW.md")
	write := func(label string) {
		t.Helper()
		body := "---\ntracker:\n  kind: memory\n  required_labels: [" + label + "]\nfae:\n  workflow_id: default\n  target_config: target\n  authorizing_message: authority\n---\nResolve {{ issue.identifier }}\n"
		if err := os.WriteFile(file, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("old")
	manager, err := symphony.NewConfigManager(file)
	if err != nil {
		t.Fatal(err)
	}
	old := newFakeLinearAdapter()
	old.putIssue(symphony.Issue{ID: "old", Identifier: "ENG-1", Title: "old scope", State: "Todo", Dispatchable: true, Labels: []string{"old"}})
	bridge := &linearStateFixture{statuses: map[string]LinearTaskStatus{}}
	rebuilt := 0
	o := NewLinearOrchestrator(OrchestratorConfig{Adapter: old, Bridge: bridge, ConfigManager: manager, ServiceConfig: *manager.Config(), AdapterFactory: func(cfg symphony.ServiceConfig) (tracker.Adapter, error) {
		rebuilt++
		a := newFakeLinearAdapter()
		a.putIssue(symphony.Issue{ID: "new", Identifier: "ENG-2", Title: "new scope", State: "Todo", Dispatchable: true, Labels: cfg.Tracker.RequiredLabels})
		return a, nil
	}})
	if err := o.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	write("new")
	if err := o.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if rebuilt != 1 || bridge.admitted != 2 || bridge.stopped != 1 || o.IssueToTask["new"] == "" {
		t.Fatal(rebuilt, bridge.admitted, bridge.stopped, o.IssueToTask)
	}
}
func (b *linearStateFixture) LookupLinearTask(_ context.Context, id string) (LinearTaskStatus, bool, error) {
	s, ok := b.statuses[id]
	return s, ok, nil
}
func (b *linearStateFixture) ListLinearTasks(context.Context) (map[string]LinearTaskStatus, error) {
	out := map[string]LinearTaskStatus{}
	for id, s := range b.statuses {
		out[id] = s
	}
	return out, nil
}

func TestLinearEmbeddedUsesDurableEngineWithoutWorkerOrImplicitWrites(t *testing.T) {
	ctx := context.Background()
	adapter := newFakeLinearAdapter()
	adapter.putIssue(symphony.Issue{ID: "issue-1", Identifier: "ENG-1", Title: "implement scoped task", State: "Todo", Dispatchable: true, Labels: []string{"fae"}})
	bridge := &linearStateFixture{statuses: map[string]LinearTaskStatus{}}
	workspace := filepath.Join(t.TempDir(), "must-not-create-symphony-workspace")
	newOrchestrator := func() *LinearOrchestrator {
		return NewLinearOrchestrator(OrchestratorConfig{Adapter: adapter, Bridge: bridge, ServiceConfig: symphony.ServiceConfig{Workspace: symphony.WorkspaceConfig{Root: workspace}, Tracker: symphony.TrackerConfig{RequiredLabels: []string{"fae"}}, Agent: symphony.AgentConfig{MaxConcurrentAgents: 1}}, WorkerLauncher: func(context.Context, symphony.Issue, symphony.Workspace, string, *int) error {
			t.Error("second coding-agent worker started")
			return nil
		}})
	}
	o := newOrchestrator()
	for range 3 {
		if err := o.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if bridge.admitted != 1 || len(o.Running) != 1 {
		t.Fatal(bridge.admitted, len(o.Running))
	}
	if len(adapter.transitions) != 0 || len(adapter.comments) != 0 {
		t.Fatal("read-only intake wrote to Linear")
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatal("embedded intake created an unrelated workspace", err)
	}
	o.Running["issue-1"].StartedAt = time.Now().Add(-24 * time.Hour)
	if err := o.Tick(ctx); err != nil || bridge.stopped != 0 {
		t.Fatal("Symphony stall logic stopped a Fae-owned task", err)
	}
	bridge.statuses["issue-1"] = LinearTaskStatus{TaskID: "task-issue-1", State: "Done", Version: 5, Terminal: true, Completed: true}
	if err := o.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(o.Running) != 0 || bridge.admitted != 1 {
		t.Fatal("completed task retained a slot or was readmitted")
	}
	restarted := newOrchestrator()
	if err := restarted.Tick(ctx); err != nil || bridge.admitted != 1 || !restarted.Completed["issue-1"] {
		t.Fatal("restart lost durable completion", err)
	}
}

func TestLinearDaemonScopeSelectionAndInvalidAdapter(t *testing.T) {
	file := filepath.Join(t.TempDir(), "WORKFLOW.md")
	body := "---\ntracker:\n  kind: linear\n  provider:\n    team_key: ENG\n    api_key: fixture-token\nfae:\n  workflow_id: default\n  target_config: target\n  authorizing_message: authority\n---\nResolve {{ issue.identifier }}\n"
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveSymphonyConfig(file, "")
	if err != nil {
		t.Fatal("team-only scope rejected", err)
	}
	if _, ok := cfg.Adapter.(*tracker.LinearAdapter); !ok {
		t.Fatal("Linear configuration silently fell back to memory")
	}
	bad := cfg.ServiceConfig
	bad.Tracker.Provider["endpoint"] = "http://linear.example.invalid/graphql"
	if _, err := buildDaemonTrackerAdapter(bad); err == nil {
		t.Fatal("invalid Linear adapter silently replaced with memory")
	}
	hook := "echo clone"
	bad.Hooks.AfterCreate = &hook
	if err := validateEmbeddedLinear(bad); err == nil {
		t.Fatal("embedded engine accepted unrelated Symphony hooks")
	}
}

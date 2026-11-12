package daemon

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/symphony"
	"github.com/xleelz/fae/workflow"
)

func linearIntakeDecisionWorkflow() workflow.Document {
	return workflow.Document{SchemaVersion: 2, ID: "linear-intake-decision", TargetRequirements: &workflow.TargetRequirements{}, Produces: "artifacts", Start: "prepare", Limits: workflow.Limits{Attempts: 8, TestRuns: 1, ScopeWidens: 1}, Roles: map[string]workflow.Role{"worker": {Instructions: "Prepare the issue's artifact", Briefs: map[string]string{"default": "Prepare a short report"}, Routes: []workflow.Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"report"}}}, Artifacts: map[string]workflow.ArtifactType{"report": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}, "decision": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}}, Stages: map[string]workflow.Stage{"prepare": {Primitive: workflow.AgentTurn, Role: "worker", Output: "report"}, "approve": {Primitive: workflow.HumanDecision, Question: "Accept this issue's prepared artifact?", Options: []workflow.DecisionOption{{ID: "accept", Next: "Done"}}, Output: "decision", OnTimeout: workflow.StayWaiting}}, Edges: []workflow.Edge{{From: "prepare", To: "approve"}, {From: "approve", To: "Done"}}}
}

func TestLinearIntakeNativeDecisionDependenciesAndRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"prepare": {{Output: json.RawMessage(`{"ready":true}`)}, {Output: json.RawMessage(`{"ready":true}`)}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	doc := linearIntakeDecisionWorkflow()
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "TargetConfigPut", TargetPutParams{ID: "linear-intake-target", Document: targetFixture(cfg.Workspace), AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	adapter := newFakeLinearAdapter()
	parent := symphony.Issue{ID: "44444444-4444-4444-8444-444444444444", Identifier: "ENG-40", Title: "Parent artifact", State: "Todo", Labels: []string{"symphony"}, Dispatchable: true, Priority: intPtr(1)}
	terminal := false
	child := symphony.Issue{ID: "55555555-5555-4555-8555-555555555555", Identifier: "ENG-41", Title: "Dependent artifact", State: "Todo", Labels: []string{"symphony"}, Dispatchable: false, Priority: intPtr(2), BlockedBy: []symphony.BlockerRef{{ID: strPtr(parent.ID), Identifier: strPtr(parent.Identifier), State: strPtr("Todo"), Terminal: &terminal}}}
	adapter.putIssue(parent)
	adapter.putIssue(child)
	bridge := d.LinearTaskBridge()
	bridge.config = symphony.FaeConfig{WorkflowID: doc.ID, TargetConfig: "linear-intake-target", AuthorizingMessage: "message", Mode: "Local", SyncState: true}
	now := time.Now()
	var extraWorkers atomic.Int32
	service := symphony.ServiceConfig{Fae: bridge.config, Tracker: symphony.TrackerConfig{RequiredLabels: []string{"symphony"}}, Workspace: symphony.WorkspaceConfig{Root: t.TempDir()}, Agent: symphony.AgentConfig{MaxConcurrentAgents: 1}, Codex: symphony.CodexConfig{StallTimeoutMS: 300000}}
	makeOrch := func() *LinearOrchestrator {
		return NewLinearOrchestrator(OrchestratorConfig{Adapter: adapter, Bridge: bridge, ServiceConfig: service, PromptTemplate: "Prepare {{ issue.identifier }}: {{ issue.title }}", Now: func() time.Time { return now }, WorkerLauncher: func(context.Context, symphony.Issue, symphony.Workspace, string, *int) error {
			extraWorkers.Add(1)
			return nil
		}})
	}
	orch := makeOrch()
	tick := func() {
		t.Helper()
		if e := orch.Tick(ctx); e != nil {
			t.Fatal(e)
		}
	}
	tick()
	parentStatus, found, err := bridge.LookupLinearTask(ctx, parent.ID)
	if err != nil || !found {
		t.Fatalf("parent %+v %t %v", parentStatus, found, err)
	}
	if _, found, err = bridge.LookupLinearTask(ctx, child.ID); err != nil || found {
		t.Fatalf("blocked child admitted: %t %v", found, err)
	}
	pending := awaitDecision(t, d, parentStatus.TaskID, 1)
	tick()
	held, found, err := bridge.LookupLinearTask(ctx, parent.ID)
	if err != nil || !found || !held.NeedsOperator || held.Terminal {
		t.Fatalf("human decision status %+v %t %v", held, found, err)
	}
	// Symphony's five-minute worker stall timeout cannot retire the FAE engine's
	// protected operator wait or start a second worker while it waits.
	now = now.Add(10 * time.Minute)
	tick()
	held, _, err = bridge.LookupLinearTask(ctx, parent.ID)
	if err != nil || !held.NeedsOperator || held.Terminal || held.State != "approve" {
		t.Fatalf("operator wait retired %+v %v", held, err)
	}
	if _, err = call(t, d, "Decide", DecideParams{ID: parentStatus.TaskID, Decision: pending.ID, Option: "accept", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, parentStatus.TaskID, "Done")
	tick()
	adapter.mu.Lock()
	parentState := adapter.issues[parent.ID].State
	adapter.mu.Unlock()
	if parentState != "Done" {
		t.Fatalf("parent FAE completion did not update fake Linear: %s", parentState)
	}
	orch.mu.RLock()
	running := len(orch.Running)
	orch.mu.RUnlock()
	if running != 0 {
		t.Fatalf("completed parent retained intake slot: %d", running)
	}
	terminal = true
	child.Dispatchable = true
	child.BlockedBy[0].State = strPtr("Done")
	child.BlockedBy[0].Terminal = &terminal
	adapter.putIssue(child)
	tick()
	childStatus, found, err := bridge.LookupLinearTask(ctx, child.ID)
	if err != nil || !found || childStatus.TaskID == parentStatus.TaskID {
		t.Fatalf("unblocked child %+v %t %v", childStatus, found, err)
	}
	childDecision := awaitDecision(t, d, childStatus.TaskID, 1)
	if _, err = call(t, d, "Decide", DecideParams{ID: childStatus.TaskID, Decision: childDecision.ID, Option: "accept", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, childStatus.TaskID, "Done")
	tick()
	baseline := map[string]controlplane.JobSnapshot{}
	for _, id := range []string{parentStatus.TaskID, childStatus.TaskID} {
		j, e := d.store.GetJob(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		baseline[id] = j
	}
	for range 3 {
		tick()
	}
	// A new orchestrator reconstructs UUID bindings and completed/claimed state
	// from Dolt; no in-memory Running/Completed state is carried over.
	orch = makeOrch()
	tick()
	tick()
	d.mu.Lock()
	graph, err := d.store.Backlog(ctx)
	d.mu.Unlock()
	if err != nil || len(graph.Items) != 2 {
		t.Fatalf("duplicate persisted intake %+v %v", graph, err)
	}
	for _, item := range graph.Items {
		before, ok := baseline[item.ID]
		if !ok {
			t.Fatalf("unexpected task %+v", item)
		}
		after, e := d.store.GetJob(ctx, item.ID)
		if e != nil || after.Version != before.Version {
			t.Fatalf("repeat poll mutated accepted source task %+v %+v %v", before, after, e)
		}
		if item.Source != linearSource(parent.ID) && item.Source != linearSource(child.ID) {
			t.Fatalf("wrong UUID source %s", item.Source)
		}
	}
	if extraWorkers.Load() != 0 {
		t.Fatalf("embedded intake launched %d extra Symphony workers", extraWorkers.Load())
	}
	adapter.mu.Lock()
	comments := len(adapter.comments)
	childState := adapter.issues[child.ID].State
	adapter.mu.Unlock()
	if comments != 0 {
		t.Fatalf("post_comments=false produced %d comments", comments)
	}
	if childState != "Done" {
		t.Fatalf("child completion not synced: %s", childState)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		view, e := d.workflowSlotSnapshot(ctx)
		d.mu.Unlock()
		if e != nil {
			t.Fatal(e)
		}
		holders := view.(map[string]any)["holders"].(map[string]scheduler.WorkflowHolder)
		if len(holders) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed workflows retained holders %+v", holders)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

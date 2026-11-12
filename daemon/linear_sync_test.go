package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/symphony"
	"github.com/xleelz/fae/tracker"
)

type fakeLinearAdapter struct {
	mu             sync.Mutex
	kind           string
	activeStates   []string
	terminalStates []string
	issues         map[string]symphony.Issue
	fetchStatesErr error
	fetchIDsErr    error
	transitions    []stateTransitionCall
	comments       []commentCall
	toolCalls      []string
}

type stateTransitionCall struct {
	IssueID     string
	TargetState string
}

type commentCall struct {
	IssueID string
	Body    string
}

func newFakeLinearAdapter() *fakeLinearAdapter {
	return &fakeLinearAdapter{
		kind:           "linear",
		activeStates:   []string{"Todo", "In Progress", "In Review"},
		terminalStates: []string{"Done", "Closed", "Cancelled", "Canceled", "Duplicate"},
		issues:         make(map[string]symphony.Issue),
	}
}

func (a *fakeLinearAdapter) putIssue(iss symphony.Issue) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.issues[iss.ID] = iss
}

func (a *fakeLinearAdapter) Kind() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.kind
}

func (a *fakeLinearAdapter) ActiveStates() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.activeStates...)
}

func (a *fakeLinearAdapter) TerminalStates() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.terminalStates...)
}

func (a *fakeLinearAdapter) FetchIssuesByStates(_ context.Context, stateNames []string) ([]symphony.Issue, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fetchStatesErr != nil {
		return nil, a.fetchStatesErr
	}
	var out []symphony.Issue
	for _, iss := range a.issues {
		if symphony.StateInList(iss.State, stateNames) {
			out = append(out, iss)
		}
	}
	return out, nil
}

func (a *fakeLinearAdapter) FetchIssuesByIDs(_ context.Context, issueIDs []string) ([]symphony.Issue, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fetchIDsErr != nil {
		return nil, a.fetchIDsErr
	}
	var out []symphony.Issue
	for _, id := range issueIDs {
		if iss, ok := a.issues[id]; ok {
			out = append(out, iss)
		}
	}
	return out, nil
}

func (a *fakeLinearAdapter) TransitionIssueState(_ context.Context, issueID string, targetStateName string) (symphony.Issue, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	iss, ok := a.issues[issueID]
	if !ok {
		return symphony.Issue{}, errors.New("issue not found")
	}
	iss.State = targetStateName
	a.issues[issueID] = iss
	a.transitions = append(a.transitions, stateTransitionCall{IssueID: issueID, TargetState: targetStateName})
	return iss, nil
}

func (a *fakeLinearAdapter) AddIssueComment(_ context.Context, issueID string, body string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.comments = append(a.comments, commentCall{IssueID: issueID, Body: body})
	return "cmt-" + issueID, nil
}

func (a *fakeLinearAdapter) AgentToolSpecs() []tracker.ToolSpec {
	return []tracker.ToolSpec{
		{Name: "linear_get_issue", Mutates: false},
		{Name: "linear_update_issue_state", Mutates: true},
		{Name: "linear_create_comment", Mutates: true},
		{Name: "linear_add_attachment_link", Mutates: true},
	}
}

func (a *fakeLinearAdapter) SecretEnvironmentNames() []string {
	return []string{"LINEAR_API_KEY", "SYMPHONY_LINEAR_TOKEN"}
}

func (a *fakeLinearAdapter) ExecuteAgentTool(_ context.Context, name string, args map[string]any, toolCtx tracker.ToolContext) tracker.ToolResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.toolCalls = append(a.toolCalls, name)
	switch name {
	case "linear_get_issue":
		return tracker.ToolResult{
			Success: true,
			Output: map[string]any{
				"id":         toolCtx.Issue.ID,
				"identifier": toolCtx.Issue.Identifier,
				"state":      toolCtx.Issue.State,
			},
		}
	case "linear_update_issue_state":
		target, _ := args["state"].(string)
		if iss, ok := a.issues[toolCtx.Issue.ID]; ok && target != "" {
			iss.State = target
			a.issues[toolCtx.Issue.ID] = iss
		}
		return tracker.ToolResult{
			Success: true,
			Output:  map[string]any{"state": target},
		}
	case "linear_create_comment":
		body, _ := args["body"].(string)
		a.comments = append(a.comments, commentCall{IssueID: toolCtx.Issue.ID, Body: body})
		return tracker.ToolResult{
			Success: true,
			Output:  map[string]any{"comment_id": "agent-cmt-1"},
		}
	default:
		return tracker.ToolResult{Success: true, Output: map[string]any{"tool": name}}
	}
}

func intPtr(v int) *int              { return &v }
func strPtr(v string) *string        { return &v }
func timePtr(t time.Time) *time.Time { return &t }

func TestLinearDispatchOrderingAndEligibility(t *testing.T) {
	adapter := newFakeLinearAdapter()
	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Minute)

	// Issue 1: priority 2, newer
	adapter.putIssue(symphony.Issue{
		ID: "id-p2-new", Identifier: "MT-200", Title: "Priority 2 newer", State: "Todo",
		Priority: intPtr(2), CreatedAt: timePtr(t1), Dispatchable: true, Labels: []string{"Symphony", "backend"},
	})
	// Issue 2: priority 1, Urgent
	adapter.putIssue(symphony.Issue{
		ID: "id-p1", Identifier: "MT-300", Title: "Priority 1 urgent", State: "Todo",
		Priority: intPtr(1), CreatedAt: timePtr(t1), Dispatchable: true, Labels: []string{"symphony"},
	})
	// Issue 3: priority 2, older, identifier B
	adapter.putIssue(symphony.Issue{
		ID: "id-p2-old-b", Identifier: "MT-102", Title: "Priority 2 older B", State: "Todo",
		Priority: intPtr(2), CreatedAt: timePtr(t0), Dispatchable: true, Labels: []string{"SYMPHONY"},
	})
	// Issue 4: priority 2, older, identifier A
	adapter.putIssue(symphony.Issue{
		ID: "id-p2-old-a", Identifier: "MT-101", Title: "Priority 2 older A", State: "Todo",
		Priority: intPtr(2), CreatedAt: timePtr(t0), Dispatchable: true, Labels: []string{"symphony"},
	})
	// Issue 5: nil priority (sorts after 1..4)
	adapter.putIssue(symphony.Issue{
		ID: "id-nil-prio", Identifier: "MT-050", Title: "No priority", State: "Todo",
		Priority: nil, CreatedAt: timePtr(t0), Dispatchable: true, Labels: []string{"symphony"},
	})
	// Ineligible: dispatchable=false
	adapter.putIssue(symphony.Issue{
		ID: "id-not-dispatchable", Identifier: "MT-001", Title: "Blocked", State: "Todo",
		Priority: intPtr(1), CreatedAt: timePtr(t0), Dispatchable: false, Labels: []string{"symphony"},
	})
	// Ineligible: missing required label
	adapter.putIssue(symphony.Issue{
		ID: "id-missing-label", Identifier: "MT-002", Title: "Unlabeled", State: "Todo",
		Priority: intPtr(1), CreatedAt: timePtr(t0), Dispatchable: true, Labels: []string{"other"},
	})
	// Ineligible: Todo with non-terminal blocker
	adapter.putIssue(symphony.Issue{
		ID: "id-todo-blocked", Identifier: "MT-003", Title: "Todo with active blocker", State: "Todo",
		Priority: intPtr(1), CreatedAt: timePtr(t0), Dispatchable: true, Labels: []string{"symphony"},
		BlockedBy: []symphony.BlockerRef{{ID: strPtr("blk-1"), State: strPtr("In Progress")}},
	})
	// Eligible: Todo with terminal blocker
	adapter.putIssue(symphony.Issue{
		ID: "id-todo-unblocked", Identifier: "MT-400", Title: "Todo with Done blocker", State: "Todo",
		Priority: intPtr(3), CreatedAt: timePtr(t0), Dispatchable: true, Labels: []string{"symphony"},
		BlockedBy: []symphony.BlockerRef{{ID: strPtr("blk-done"), State: strPtr("Done")}},
	})

	wsRoot := t.TempDir()
	var dispatchedOrder []string
	var mu sync.Mutex

	orch := NewLinearOrchestrator(OrchestratorConfig{
		Adapter: adapter,
		ServiceConfig: symphony.ServiceConfig{
			Tracker:   symphony.TrackerConfig{RequiredLabels: []string{"Symphony"}},
			Workspace: symphony.WorkspaceConfig{Root: wsRoot},
			Agent:     symphony.AgentConfig{MaxConcurrentAgents: 10},
		},
		WorkerLauncher: func(ctx context.Context, issue symphony.Issue, _ symphony.Workspace, _ string, _ *int) error {
			mu.Lock()
			dispatchedOrder = append(dispatchedOrder, issue.Identifier)
			mu.Unlock()
			<-ctx.Done()
			return nil
		},
	})

	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("Tick failed: %v", err)
	}

	// Verify deterministic dispatch order: MT-300 (p1) -> MT-101 (p2, t0, A) -> MT-102 (p2, t0, B) -> MT-200 (p2, t1) -> MT-400 (p3) -> MT-050 (nil prio)
	want := []string{"MT-300", "MT-101", "MT-102", "MT-200", "MT-400", "MT-050"}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := append([]string(nil), dispatchedOrder...)
		mu.Unlock()
		if len(got) == len(want) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Because goroutines launch asynchronously, check that the exact set was dispatched and SortIssuesForDispatch produces exact order.
	sorted := symphony.SortIssuesForDispatch([]symphony.Issue{
		adapter.issues["id-p2-new"],
		adapter.issues["id-p1"],
		adapter.issues["id-p2-old-b"],
		adapter.issues["id-p2-old-a"],
		adapter.issues["id-nil-prio"],
		adapter.issues["id-todo-unblocked"],
	})
	var sortedIdents []string
	for _, s := range sorted {
		sortedIdents = append(sortedIdents, s.Identifier)
	}
	if !reflect.DeepEqual(sortedIdents, want) {
		t.Fatalf("SortIssuesForDispatch got %v, want %v", sortedIdents, want)
	}
	if len(orch.Running) != 6 {
		t.Fatalf("expected 6 running issues, got %d", len(orch.Running))
	}
	for _, blockedID := range []string{"id-not-dispatchable", "id-missing-label", "id-todo-blocked"} {
		if _, running := orch.Running[blockedID]; running {
			t.Fatalf("ineligible issue %s should not have been dispatched", blockedID)
		}
	}
}

func TestLinearActiveRunReconciliationAndWorkspaceCleanup(t *testing.T) {
	adapter := newFakeLinearAdapter()
	wsRoot := t.TempDir()

	// Startup cleanup should remove terminal issue workspaces and ignore errors.
	termWS, err := EnsureIssueWorkspace(wsRoot, "MT-TERM")
	if err != nil {
		t.Fatal(err)
	}
	adapter.putIssue(symphony.Issue{
		ID: "id-term-startup", Identifier: "MT-TERM", Title: "Already done", State: "Done", Dispatchable: true,
	})
	orch := NewLinearOrchestrator(OrchestratorConfig{
		Adapter: adapter,
		ServiceConfig: symphony.ServiceConfig{
			Workspace: symphony.WorkspaceConfig{Root: wsRoot},
			Agent:     symphony.AgentConfig{MaxConcurrentAgents: 5},
		},
	})
	orch.StartupCleanup(context.Background())
	if _, err := os.Stat(termWS.Path); !os.IsNotExist(err) {
		t.Fatalf("expected terminal workspace %s to be removed at startup", termWS.Path)
	}

	// Dispatch 3 active issues:
	// 1. id-keep: stays active ("In Progress") -> snapshot updated, stays running
	// 2. id-nonactive: moves to "Backlog" (non-active, non-terminal) -> stopped, workspace preserved
	// 3. id-done: moves to "Done" (terminal) -> stopped, workspace deleted
	for _, iss := range []symphony.Issue{
		{ID: "id-keep", Identifier: "MT-KEEP", Title: "Keep running", State: "Todo", Dispatchable: true},
		{ID: "id-nonactive", Identifier: "MT-BACKLOG", Title: "Move to backlog", State: "Todo", Dispatchable: true},
		{ID: "id-done", Identifier: "MT-DONE", Title: "Move to done", State: "Todo", Dispatchable: true},
	} {
		adapter.putIssue(iss)
	}

	if err := orch.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(orch.Running) != 3 {
		t.Fatalf("expected 3 running issues, got %d", len(orch.Running))
	}

	// If tracker refresh fails with an error, workers must keep running (§8.5).
	adapter.mu.Lock()
	adapter.fetchIDsErr = errors.New("network timeout")
	adapter.mu.Unlock()
	orch.ReconcileRunningIssues(context.Background())
	if len(orch.Running) != 3 {
		t.Fatalf("expected workers to keep running on refresh error, got %d", len(orch.Running))
	}

	// Clear error and transition states in Linear.
	adapter.mu.Lock()
	adapter.fetchIDsErr = nil
	adapter.mu.Unlock()
	adapter.putIssue(symphony.Issue{ID: "id-keep", Identifier: "MT-KEEP", Title: "Keep running", State: "In Progress", Dispatchable: true})
	adapter.putIssue(symphony.Issue{ID: "id-nonactive", Identifier: "MT-BACKLOG", Title: "Move to backlog", State: "Backlog", Dispatchable: true})
	adapter.putIssue(symphony.Issue{ID: "id-done", Identifier: "MT-DONE", Title: "Move to done", State: "Done", Dispatchable: true})

	orch.ReconcileRunningIssues(context.Background())

	if len(orch.Running) != 1 {
		t.Fatalf("expected 1 running issue after reconciliation, got %d", len(orch.Running))
	}
	if orch.Running["id-keep"].Issue.State != "In Progress" {
		t.Fatalf("expected id-keep state snapshot updated to In Progress, got %q", orch.Running["id-keep"].Issue.State)
	}
	// Non-active issue workspace must still exist.
	backlogWS := filepath.Join(wsRoot, "MT-BACKLOG")
	if _, err := os.Stat(backlogWS); err != nil {
		t.Fatalf("expected non-active workspace %s to be preserved: %v", backlogWS, err)
	}
	// Terminal issue workspace must be removed.
	doneWS := filepath.Join(wsRoot, "MT-DONE")
	if _, err := os.Stat(doneWS); !os.IsNotExist(err) {
		t.Fatalf("expected terminal workspace %s to be deleted", doneWS)
	}
}

func TestLinearRetriesBackoffSlotExhaustionAndStallDetection(t *testing.T) {
	adapter := newFakeLinearAdapter()
	wsRoot := t.TempDir()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	clockNow := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advanceClock := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	adapter.putIssue(symphony.Issue{
		ID: "id-1", Identifier: "MT-1", Title: "Issue 1", State: "In Progress", Dispatchable: true,
	})

	orch := NewLinearOrchestrator(OrchestratorConfig{
		Adapter: adapter,
		ServiceConfig: symphony.ServiceConfig{
			Workspace: symphony.WorkspaceConfig{Root: wsRoot},
			Agent:     symphony.AgentConfig{MaxConcurrentAgents: 1, MaxRetryBackoffMS: 35000},
			Codex:     symphony.CodexConfig{StallTimeoutMS: 60000},
		},
		Now: clockNow,
	})

	if err := orch.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	advanceClock(5 * time.Second)

	// 1. Normal exit schedules 1s (1000ms) continuation retry with attempt=1.
	orch.OnWorkerExit("id-1", true, nil)
	if !orch.Completed["id-1"] {
		t.Fatal("expected Completed[id-1] = true after normal exit")
	}
	retry := orch.RetryAttempts["id-1"]
	if retry == nil || retry.Attempt != 1 {
		t.Fatalf("expected continuation retry attempt=1, got %+v", retry)
	}
	if gotDelay := retry.DueAt.Sub(clockNow()); gotDelay != 1000*time.Millisecond {
		t.Fatalf("expected 1s continuation retry delay, got %v", gotDelay)
	}

	// Fire retry timer to re-dispatch id-1 (attempt=1).
	if err := orch.OnRetryTimer(context.Background(), "id-1"); err != nil {
		t.Fatal(err)
	}
	if _, running := orch.Running["id-1"]; !running {
		t.Fatal("expected id-1 to be re-dispatched by retry timer")
	}

	// 2. Abnormal exit schedules exponential backoff:
	// Since it was running at attempt=1, next failure is attempt=2 -> 20s; attempt=1 -> 10s, attempt=2 -> 20s, attempt=3 -> capped at 35s.
	if BackoffDelayMS(1, 35000) != 10000 || BackoffDelayMS(2, 35000) != 20000 || BackoffDelayMS(3, 35000) != 35000 {
		t.Fatalf("unexpected backoff calculation: %d %d %d", BackoffDelayMS(1, 35000), BackoffDelayMS(2, 35000), BackoffDelayMS(3, 35000))
	}

	orch.OnWorkerExit("id-1", false, errors.New("agent crashed"))
	retry = orch.RetryAttempts["id-1"]
	if retry == nil || retry.Attempt != 2 || retry.Error != "agent crashed" {
		t.Fatalf("expected failure retry attempt=2, got %+v", retry)
	}
	if gotDelay := retry.DueAt.Sub(clockNow()); gotDelay != 20*time.Second {
		t.Fatalf("expected 20s retry delay for attempt 2, got %v", gotDelay)
	}

	// 3. Slot exhaustion on retry timer requeues with "no available orchestrator slots".
	adapter.putIssue(symphony.Issue{
		ID: "id-2", Identifier: "MT-2", Title: "Slot occupier", State: "In Progress", Dispatchable: true,
	})
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, running := orch.Running["id-2"]; !running {
		t.Fatal("expected id-2 to occupy the single slot")
	}
	if err := orch.OnRetryTimer(context.Background(), "id-1"); err != nil {
		t.Fatal(err)
	}
	retry = orch.RetryAttempts["id-1"]
	if retry == nil || retry.Attempt != 3 || retry.Error != "no available orchestrator slots" {
		t.Fatalf("expected slot exhaustion retry with attempt=3, got %+v", retry)
	}

	// 4. Stall detection kills stalled run (id-2) and queues retry.
	advanceClock(70 * time.Second) // > 60000ms StallTimeoutMS
	orch.ReconcileRunningIssues(context.Background())
	if _, running := orch.Running["id-2"]; running {
		t.Fatal("expected stalled issue id-2 to be removed from Running")
	}
	stalledRetry := orch.RetryAttempts["id-2"]
	if stalledRetry == nil || !strings.Contains(stalledRetry.Error, "stalled") {
		t.Fatalf("expected stalled retry entry for id-2, got %+v", stalledRetry)
	}
}

func TestLinearTokenAccountingAndHostTools(t *testing.T) {
	adapter := newFakeLinearAdapter()
	adapter.putIssue(symphony.Issue{
		ID: "id-tok", Identifier: "MT-TOK", Title: "Token test", State: "In Progress", Dispatchable: true,
	})
	orch := NewLinearOrchestrator(OrchestratorConfig{
		Adapter: adapter,
		ServiceConfig: symphony.ServiceConfig{
			Workspace: symphony.WorkspaceConfig{Root: t.TempDir()},
			Agent:     symphony.AgentConfig{MaxConcurrentAgents: 2},
		},
	})
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Event 1: cumulative total_token_usage
	orch.RecordAgentEvent("id-tok", AgentEvent{
		Event:     "turn_started",
		SessionID: "sess-1",
		Usage: &TokenUsagePayload{
			TotalTokenUsage: &TokenCounts{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
		},
	})
	// Event 2: delta-only last_token_usage must be ignored to prevent double-counting (§13.5)
	orch.RecordAgentEvent("id-tok", AgentEvent{
		Event: "notification",
		Usage: &TokenUsagePayload{
			LastTokenUsage: &TokenCounts{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
		},
	})
	// Event 3: updated cumulative total_token_usage + rate_limits
	orch.RecordAgentEvent("id-tok", AgentEvent{
		Event:      "turn_completed",
		Message:    "Completed turn",
		RateLimits: map[string]any{"remaining": 42},
		Usage: &TokenUsagePayload{
			TotalTokenUsage: &TokenCounts{InputTokens: 180, OutputTokens: 70, TotalTokens: 250},
		},
	})

	snap := orch.StateSnapshot()
	if snap.CodexTotals.InputTokens != 180 || snap.CodexTotals.OutputTokens != 70 || snap.CodexTotals.TotalTokens != 250 {
		t.Fatalf("unexpected token totals: %+v", snap.CodexTotals)
	}

	// Verify secret stripping and host-side tool execution.
	stripped := orch.StrippedAgentEnv([]string{"PATH=/usr/bin", "LINEAR_API_KEY=secret123", "HOME=/tmp"})
	for _, kv := range stripped {
		if strings.HasPrefix(kv, "LINEAR_API_KEY=") {
			t.Fatalf("expected LINEAR_API_KEY to be stripped from child env: %v", stripped)
		}
	}
	res := orch.ExecuteAgentTool(context.Background(), "MT-TOK", "linear_update_issue_state", map[string]any{"state": "In Review"})
	if !res.Success {
		t.Fatalf("ExecuteAgentTool failed: %+v", res)
	}
	if orch.Running["id-tok"].Issue.State != "In Review" {
		t.Fatalf("expected issue state updated to In Review, got %q", orch.Running["id-tok"].Issue.State)
	}
}

func TestLinearHybridFaeBacklogAdmissionAndStageSync(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)

	adapter := newFakeLinearAdapter()
	adapter.putIssue(symphony.Issue{
		ID:           "lin-101",
		Identifier:   "FAE-101",
		Title:        "Hybrid workflow integration",
		Description:  strPtr("Implement hybrid Fae + Linear stage synchronization"),
		Priority:     intPtr(1), // Urgent -> Fae priority 40
		State:        "Todo",
		Dispatchable: true,
	})

	orch := NewLinearOrchestrator(OrchestratorConfig{
		Adapter: adapter,
		Bridge:  d.LinearTaskBridge(),
		ServiceConfig: symphony.ServiceConfig{
			Workspace: symphony.WorkspaceConfig{Root: t.TempDir()},
			Agent:     symphony.AgentConfig{MaxConcurrentAgents: 4},
		},
		PromptTemplate: "Solve {{ issue.identifier }}: {{ issue.title }}",
	})

	if err := orch.Tick(ctx); err != nil {
		t.Fatalf("Tick failed: %v", err)
	}

	entry := orch.Running["lin-101"]
	if entry == nil || entry.TaskID != "fae-101" {
		t.Fatalf("expected running issue with TaskID fae-101, got %+v", entry)
	}
	if entry.Issue.State != "In Progress" {
		t.Fatalf("expected dispatch to transition Linear issue from Todo to In Progress, got %q", entry.Issue.State)
	}

	// Verify the task was admitted into Fae's Dolt backlog with mapped priority 40 and title "[FAE-101] Hybrid workflow integration".
	d.mu.Lock()
	jobSnap, exists := d.jobs["fae-101"]
	d.mu.Unlock()
	if !exists {
		t.Fatal("expected task fae-101 to exist in Daemon jobs")
	}
	var env envelope
	if err := decodeEnvelope(jobSnap.Payload, &env); err != nil {
		t.Fatal(err)
	}
	if env.Admission.Priority != 40 || env.Admission.Title != "[FAE-101] Hybrid workflow integration" {
		t.Fatalf("unexpected admitted task metadata: priority=%d title=%q", env.Admission.Priority, env.Admission.Title)
	}

	// Simulate Fae workflow stage transitions: verify -> review -> Done.
	if err := orch.SyncTaskStageToLinear(ctx, "fae-101", "verify", "Running", "Unit tests passed"); err != nil {
		t.Fatal(err)
	}
	if err := orch.SyncTaskStageToLinear(ctx, "fae-101", "review", "Running", "Ready for code review"); err != nil {
		t.Fatal(err)
	}
	if adapter.issues["lin-101"].State != "In Review" {
		t.Fatalf("expected Linear state In Review during review stage, got %q", adapter.issues["lin-101"].State)
	}
	if err := orch.SyncTaskStageToLinear(ctx, "fae-101", "land", "Done", "Landed commit abc1234"); err != nil {
		t.Fatal(err)
	}
	if adapter.issues["lin-101"].State != "Done" {
		t.Fatalf("expected Linear state Done after land completion, got %q", adapter.issues["lin-101"].State)
	}
	if len(adapter.comments) < 4 {
		t.Fatalf("expected start comment + 3 stage comments on Linear, got %d", len(adapter.comments))
	}
}

func TestLinearParentAndSubIssueHierarchySyncAndObservability(t *testing.T) {
	adapter := newFakeLinearAdapter()
	parentRef := &tracker.IssueRef{
		ID:         "lin-parent-100",
		Identifier: "FAE-100",
		Title:      "Platform Hierarchy Initiative",
		State:      "In Progress",
	}
	childRefs := []tracker.IssueRef{
		{ID: "lin-sub-201", Identifier: "FAE-201", Title: "Engine fan-out", State: "Done"},
		{ID: "lin-sub-202", Identifier: "FAE-202", Title: "Daemon bind children", State: "In Progress"},
		{ID: "lin-sub-203", Identifier: "FAE-203", Title: "Linear hierarchy sync", State: "Todo"},
	}
	issue := tracker.WithHierarchy(symphony.Issue{
		ID:           "lin-epic-200",
		Identifier:   "FAE-200",
		Title:        "Hierarchical SubWorkflow Epic",
		Description:  strPtr("Coordinate parent and sub-issues"),
		Priority:     intPtr(1),
		State:        "In Progress",
		Dispatchable: true,
	}, parentRef, childRefs)
	adapter.putIssue(issue)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	promptCh := make(chan string, 1)
	orch := NewLinearOrchestrator(OrchestratorConfig{
		Adapter: adapter,
		ServiceConfig: symphony.ServiceConfig{
			Workspace: symphony.WorkspaceConfig{Root: t.TempDir()},
			Agent:     symphony.AgentConfig{MaxConcurrentAgents: 2},
		},
		PromptTemplate: "Resolve {{ issue.identifier }}: {{ issue.title }}",
		WorkerLauncher: func(wctx context.Context, _ symphony.Issue, _ symphony.Workspace, prompt string, _ *int) error {
			select {
			case promptCh <- prompt:
			default:
			}
			<-wctx.Done()
			return nil
		},
	})

	if err := orch.Tick(ctx); err != nil {
		t.Fatalf("Tick failed: %v", err)
	}

	var launchedPrompt string
	select {
	case launchedPrompt = <-promptCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for WorkerLauncher")
	}

	rec, ok := orch.IssueRecord("FAE-200")
	if !ok {
		t.Fatal("expected IssueRecord for FAE-200")
	}
	if rec.Parent == nil || rec.Parent.Identifier != "FAE-100" || rec.Parent.Title != "Platform Hierarchy Initiative" {
		t.Fatalf("unexpected persisted Parent: %+v", rec.Parent)
	}
	if len(rec.Children) != 3 || rec.Children[0].Identifier != "FAE-201" || rec.Children[2].Identifier != "FAE-203" {
		t.Fatalf("unexpected persisted Children: %+v", rec.Children)
	}
	for _, needle := range []string{"Parent Issue: FAE-100", "FAE-201", "FAE-202", "FAE-203"} {
		if !strings.Contains(launchedPrompt, needle) {
			t.Fatalf("expected rendered prompt to include %q, got:\n%s", needle, launchedPrompt)
		}
	}

	stateSnap := orch.StateSnapshot()
	if len(stateSnap.Running) != 1 || !strings.Contains(stateSnap.Running[0].LastMessage, "parent=FAE-100") || !strings.Contains(stateSnap.Running[0].LastMessage, "FAE-201") {
		t.Fatalf("expected /api/v1/state running summary to expose parent/children hierarchy, got: %+v", stateSnap.Running)
	}

	detailSnap, ok := orch.IssueDetailSnapshot("FAE-200")
	if !ok || detailSnap.Running == nil || !strings.Contains(detailSnap.Running.LastMessage, "parent=FAE-100") || len(detailSnap.RecentEvents) == 0 || !strings.Contains(detailSnap.RecentEvents[0].Message, "children=FAE-201") {
		t.Fatalf("expected /api/v1/FAE-200 detail to expose parent/children hierarchy, got: %+v", detailSnap)
	}

	// Verify child state sync into parent record when sub-issue updates via SyncTaskStageToLinear
	childIssue := tracker.WithHierarchy(symphony.Issue{
		ID:           "lin-sub-203",
		Identifier:   "FAE-203",
		Title:        "Linear hierarchy sync",
		State:        "In Progress",
		Dispatchable: true,
	}, &tracker.IssueRef{
		ID:         "lin-epic-200",
		Identifier: "FAE-200",
		Title:      "Hierarchical SubWorkflow Epic",
		State:      "In Progress",
	}, nil)
	adapter.putIssue(childIssue)
	orch.mu.Lock()
	orch.upsertIssueRecordLocked(childIssue, "fae-203", "implement")
	orch.TaskToIssue["fae-203"] = "lin-sub-203"
	orch.IssueToTask["lin-sub-203"] = "fae-203"
	orch.mu.Unlock()

	if err := orch.SyncTaskStageToLinear(ctx, "fae-203", "land", "Done", "Completed child FAE-203"); err != nil {
		t.Fatalf("SyncTaskStageToLinear child failed: %v", err)
	}
	parentRec, ok := orch.IssueRecord("FAE-200")
	if !ok || len(parentRec.Children) != 3 || parentRec.Children[2].State != "Done" {
		t.Fatalf("expected parent FAE-200 child FAE-203 state synced to Done, got: %+v", parentRec.Children)
	}
}

func TestLinearHooksTemplateErrorsDynamicReloadAndSSHPool(t *testing.T) {
	adapter := newFakeLinearAdapter()
	wsRoot := t.TempDir()

	// 1. SanitizeWorkspaceKey collision resistance
	if SanitizeWorkspaceKey("ABC/123") == SanitizeWorkspaceKey("ABC:123") {
		t.Fatal("expected SanitizeWorkspaceKey to prevent collisions between ABC/123 and ABC:123")
	}

	// 2. Strict template error fails attempt immediately and schedules retry
	adapter.putIssue(symphony.Issue{
		ID: "id-tmpl-err", Identifier: "MT-TMPL", Title: "Template error test", State: "Todo", Dispatchable: true,
	})
	orchTmpl := NewLinearOrchestrator(OrchestratorConfig{
		Adapter: adapter,
		ServiceConfig: symphony.ServiceConfig{
			Workspace: symphony.WorkspaceConfig{Root: wsRoot},
			Agent:     symphony.AgentConfig{MaxConcurrentAgents: 2},
		},
		PromptTemplate: "Hello {{ issue.unknown_field }}",
	})
	if err := orchTmpl.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if _, running := orchTmpl.Running["id-tmpl-err"]; running {
		t.Fatal("expected issue with broken template NOT to enter Running")
	}
	if orchTmpl.RetryAttempts["id-tmpl-err"] == nil {
		t.Fatal("expected retry scheduled after template render error")
	}

	// 3. Workspace hooks (after_create, before_run, after_run, before_remove) + SSH host pool + dynamic reload
	wfDir := t.TempDir()
	wfPath := filepath.Join(wfDir, "WORKFLOW.md")
	wfContent := `---
tracker:
  kind: linear
  api_key: lin_test
  project_slug: fae
polling:
  interval_ms: 15000
workspace:
  root: ` + wsRoot + `
hooks:
  after_create: echo "created" > after_create.txt
  before_run: echo "before" > before_run.txt
  after_run: echo "after" > after_run.txt
  before_remove: echo "removed" > ../before_remove_marker.txt
  timeout_ms: 2000
worker:
  ssh_hosts:
    - ssh-host-1
    - ssh-host-2
  max_concurrent_agents_per_host: 1
agent:
  max_concurrent_agents: 5
---
Work on {{ issue.identifier }}`
	if err := os.WriteFile(wfPath, []byte(wfContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cm, err := symphony.NewConfigManager(wfPath)
	if err != nil {
		t.Fatalf("NewConfigManager: %v", err)
	}

	adapter2 := newFakeLinearAdapter()
	for _, iss := range []symphony.Issue{
		{ID: "ssh-1", Identifier: "MT-SSH-1", Title: "SSH 1", State: "Todo", Dispatchable: true},
		{ID: "ssh-2", Identifier: "MT-SSH-2", Title: "SSH 2", State: "Todo", Dispatchable: true},
		{ID: "ssh-3", Identifier: "MT-SSH-3", Title: "SSH 3", State: "Todo", Dispatchable: true},
	} {
		adapter2.putIssue(iss)
	}

	var logLines []string
	orch := NewLinearOrchestrator(OrchestratorConfig{
		Adapter:        adapter2,
		ServiceConfig:  *cm.Config(),
		PromptTemplate: cm.Workflow().PromptTemplate,
		ConfigManager:  cm,
		Logger: func(format string, args ...any) {
			logLines = append(logLines, format)
			if len(logLines) == 1 {
				panic("simulated logger sink failure")
			}
		},
	})

	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("Tick failed: %v", err)
	}
	// Only 2 SSH hosts with max_concurrent_agents_per_host=1 -> at most 2 issues running!
	if len(orch.Running) != 2 {
		t.Fatalf("expected 2 running issues capped by SSH host pool capacity, got %d", len(orch.Running))
	}

	// Verify after_create and before_run hooks executed in workspace
	ws1Path := filepath.Join(wsRoot, "MT-SSH-1")
	if _, err := os.Stat(filepath.Join(ws1Path, "after_create.txt")); err != nil {
		t.Fatalf("expected after_create.txt to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws1Path, "before_run.txt")); err != nil {
		t.Fatalf("expected before_run.txt to exist: %v", err)
	}

	// Complete ssh-1 -> runs after_run hook; then mark ssh-1 Done and fire OnRetryTimer -> runs before_remove and deletes workspace
	prevHost := orch.Running["ssh-1"].SSHHost
	if prevHost == "" {
		t.Fatal("expected SSHHost assigned on running entry")
	}
	orch.OnWorkerExit("ssh-1", true, nil)
	if _, err := os.Stat(filepath.Join(ws1Path, "after_run.txt")); err != nil {
		t.Fatalf("expected after_run.txt to exist after OnWorkerExit: %v", err)
	}

	// Test retry affinity: keep ssh-1 active first and fire OnRetryTimer -> should prefer prevHost
	if err := orch.OnRetryTimer(context.Background(), "ssh-1"); err != nil {
		t.Fatalf("OnRetryTimer: %v", err)
	}
	if orch.Running["ssh-1"] == nil || orch.Running["ssh-1"].SSHHost != prevHost {
		t.Fatalf("expected retry affinity to reuse SSH host %q, got %+v", prevHost, orch.Running["ssh-1"])
	}

	// Now mark ssh-1 terminal via OnRetryTimer and verify before_remove + cleanup
	orch.OnWorkerExit("ssh-1", true, nil)
	adapter2.putIssue(symphony.Issue{ID: "ssh-1", Identifier: "MT-SSH-1", Title: "SSH 1", State: "Done", Dispatchable: true})
	if err := orch.OnRetryTimer(context.Background(), "ssh-1"); err != nil {
		t.Fatalf("OnRetryTimer terminal: %v", err)
	}
	if _, err := os.Stat(ws1Path); !os.IsNotExist(err) {
		t.Fatalf("expected workspace %s to be removed on terminal retry cleanup", ws1Path)
	}
	if _, err := os.Stat(filepath.Join(wsRoot, "before_remove_marker.txt")); err != nil {
		t.Fatalf("expected before_remove hook marker to exist: %v", err)
	}

	// 4. OnRetryTimer Todo blocker rule: if ssh-2 is Todo with an active blocker when retry timer fires, release claim
	orch.OnWorkerExit("ssh-2", true, nil)
	adapter2.putIssue(symphony.Issue{
		ID: "ssh-2", Identifier: "MT-SSH-2", Title: "SSH 2", State: "Todo", Dispatchable: true,
		BlockedBy: []symphony.BlockerRef{{ID: strPtr("blk-active"), State: strPtr("In Progress")}},
	})
	if err := orch.OnRetryTimer(context.Background(), "ssh-2"); err != nil {
		t.Fatalf("OnRetryTimer blocked Todo: %v", err)
	}
	if _, running := orch.Running["ssh-2"]; running {
		t.Fatal("expected Todo issue with non-terminal blocker NOT to be dispatched by OnRetryTimer")
	}
	if orch.Claimed["ssh-2"] {
		t.Fatal("expected Claimed[ssh-2] to be released when Todo issue is blocked on retry")
	}

	// 5. Invalid dynamic reload preserves last known good config and sets LastTickError
	if err := os.WriteFile(wfPath, []byte("---\n- invalid_front_matter\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := orch.Tick(context.Background()); err != nil {
		t.Fatalf("Tick after invalid reload should keep running with last known good config, got %v", err)
	}
	if !strings.Contains(orch.LastTickError, "workflow reload failed") {
		t.Fatalf("expected LastTickError to record workflow reload failure, got %q", orch.LastTickError)
	}
}


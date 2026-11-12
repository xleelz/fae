package tui

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/tui/client"
)

// extFakeDaemon embeds *fakeDaemon and implements ExtendedDaemon for Wave 21 tests.
type extFakeDaemon struct {
	*fakeDaemon
	pools    []client.ProviderPoolView
	health   []client.ProviderBindingHealthView
	pauses   []client.DispatchPauseView
	panes    []client.PaneSessionView
	analysis map[string]client.SessionAnalysisView
	targets  []client.TargetConfigView
	events   []client.EventView
	prov     map[string][]client.ProvenanceEntryView
	land     map[string]client.LandStatusView
	symphony client.SymphonyStateView

	notifies     []client.NotifyParams
	followUps    []client.FollowUpParams
	poolResizes  []client.PoolResizeParams
	quotaSets    []client.QuotaSetParams
	quotaClears  []client.QuotaClearParams
	pauseCalls   []client.DispatchPauseParams
	unpauseCalls []client.DispatchPauseParams
	decides      []client.DecideParams
	resolves     []client.ResolveUncertainParams
	lifecycles   []client.LifecycleParams
	symRefreshes int
}

func extFixture() *extFakeDaemon {
	base := fixture()
	at := func(minutes int) time.Time { return fixedNow.Add(time.Duration(minutes) * time.Minute) }

	parserDetail := base.details["parser"]
	parserDetail.FollowUpOf = "lib"
	parserDetail.TurnBudget = &client.TurnBudgetView{
		WallSeconds:       600,
		MaxAssistantTurns: 12,
		OnExhaustion:      "park",
	}
	parserDetail.ReadOnlyFiles = []string{"docs/SPEC.md", "api/schema.json"}
	parserDetail.Reads = &client.ReadPolicyView{
		Allow: []string{"ext-docs/**"},
		Deny:  []string{"secrets/**"},
	}
	parserDetail.DenyPaths = []string{".env"}
	parserDetail.ApprovedScope = []string{"src/extra.x"}
	parserDetail.PendingRequest = &client.PendingRequestView{
		Kind:   "scope-widen",
		Paths:  []string{"src/ast.x"},
		Reason: "need AST node definitions",
	}
	parserDetail.TurnCheckpoints = map[string]client.TurnCheckpointView{
		"implement": {
			ProviderSession: "sess-parser-99",
			WorktreeTree:    "tree-abc123",
			Stage:           "implement",
			AssistantTurns:  5,
			WallSeconds:     180,
			UpdatedAt:       at(-2),
		},
	}
	parserDetail.Fan = &client.FanView{
		Stage:    "implement",
		Join:     "all",
		Parallel: true,
		Branches: []client.FanBranchView{
			{ID: "b-lexer", Status: "Running", Stage: "code", InFlight: true, WriteScope: []string{"src/lexer.x"}, CommitSHA: "deadbeef"},
			{ID: "b-ast", Status: "Done", Stage: "code", WriteScope: []string{"src/ast.x"}, CommitSHA: "cafebabe"},
		},
	}
	parserDetail.Notifications = []client.NotificationView{
		{
			ID:          "notif-1",
			Kind:        "breaking-change",
			Delivery:    "next-turn",
			Summary:     "token struct renamed",
			FromTask:    "lib",
			CreatedAt:   at(-15),
			DeliveredAt: at(-10),
		},
	}
	base.details["parser"] = parserDetail

	return &extFakeDaemon{
		fakeDaemon: base,
		pools: []client.ProviderPoolView{
			{
				ID:                  "claude-main",
				Provider:            "claude",
				CredentialIdentity:  "acct-primary",
				Capacity:            4,
				AdmissionLimit:      4,
				Holders:             []string{"parser", "api"},
				Waiting:             1,
				QuotaResetAt:        at(25),
				QuotaExhaustedUntil: at(25),
				CutOffTaskID:        "docs",
				QuotaReason:         "rate-limit-window",
			},
		},
		health: []client.ProviderBindingHealthView{
			{
				Provider:        "claude",
				Model:           "claude-opus-4-6",
				BindingID:       "bind-claude-1",
				Score:           0.95,
				EffectiveScore:  0.80,
				Eligible:        true,
				Outcome:         "ok",
				RateLimitProbes: 2,
				NextProbeAt:     at(5),
			},
		},
		pauses: []client.DispatchPauseView{
			{
				Scope:     "workflow",
				Target:    "default",
				Paused:    true,
				Actor:     "operator",
				Reason:    "investigating flaky test",
				UpdatedAt: at(-10),
			},
		},
		panes: []client.PaneSessionView{
			{
				ID:        "pane-1",
				TaskID:    "parser",
				Role:      "coder",
				Stage:     "implement",
				Provider:  "claude",
				Model:     "claude-opus-4-6",
				PID:       9001,
				StartedAt: at(-8),
			},
		},
		targets: []client.TargetConfigView{
			{
				ID:       "default-target",
				Revision: 2,
				ReadRoots: map[string]client.ReadRootView{
					"ext-docs": {Name: "ext-docs", Path: "/opt/docs", Commit: "11223344", Deny: []string{"private/**"}},
				},
				DenyPaths:      []string{"secrets/**", ".env"},
				DocumentGlobs:  []string{"docs/**/*.md"},
				DocumentChecks: []string{"links", "headings"},
				PushMode:       "ff-only",
				PushRemote:     "origin",
				PushBranch:     "main",
			},
		},
		events: []client.EventView{
			{
				ID:      101,
				Kind:    "scope-widen-requested",
				Task:    "parser",
				Actor:   "agent",
				At:      at(-3),
				Summary: "requested src/ast.x",
			},
		},
		symphony: client.SymphonyStateView{
			Running: []client.LinearIssueView{
				{
					ID:           "iss-1",
					Identifier:   "FAE-101",
					Title:        "Parser streaming support",
					State:        "In Progress",
					Priority:     1,
					Dispatchable: true,
					TaskID:       "parser",
					Stage:        "implement",
					TurnCount:    3,
					TokensTotal:  14500,
				},
			},
			Retrying: []client.LinearIssueView{
				{
					ID:           "iss-2",
					Identifier:   "FAE-102",
					Title:        "Docs generator",
					State:        "Todo",
					Priority:     2,
					Dispatchable: false,
					TaskID:       "docs",
					RetryAttempt: 2,
					RetryDueAt:   at(4),
					LastError:    "rate limit exceeded",
					BlockedBy:    []string{"FAE-101"},
				},
			},
			Outbox: client.TicketOutboxView{
				Pending:          2,
				OldestAgeSeconds: 18.5,
				MaxAttempts:      1,
			},
			TotalTokens:    14500,
			PollIntervalMs: 30000,
		},
	}
}

func (e *extFakeDaemon) Leases(context.Context) ([]client.ProviderPoolView, error) {
	return e.pools, e.record("Leases")
}
func (e *extFakeDaemon) ProviderHealth(context.Context) ([]client.ProviderBindingHealthView, error) {
	return e.health, e.record("ProviderHealth")
}
func (e *extFakeDaemon) DispatchPauses(context.Context) ([]client.DispatchPauseView, error) {
	return e.pauses, e.record("DispatchPauses")
}
func (e *extFakeDaemon) Panes(context.Context) ([]client.PaneSessionView, error) {
	return e.panes, e.record("Panes")
}
func (e *extFakeDaemon) AnalyzeSession(_ context.Context, taskID string) (client.SessionAnalysisView, error) {
	return e.analysis[taskID], e.record("AnalyzeSession")
}
func (e *extFakeDaemon) TargetConfigs(context.Context) ([]client.TargetConfigView, error) {
	return e.targets, e.record("TargetConfigs")
}
func (e *extFakeDaemon) Events(context.Context, int) ([]client.EventView, error) {
	return e.events, e.record("Events")
}
func (e *extFakeDaemon) ProvenanceChain(_ context.Context, taskID string) ([]client.ProvenanceEntryView, error) {
	return e.prov[taskID], e.record("ProvenanceChain")
}
func (e *extFakeDaemon) LandStatus(_ context.Context, taskID string) (client.LandStatusView, error) {
	return e.land[taskID], e.record("LandStatus")
}
func (e *extFakeDaemon) SymphonyState(context.Context) (client.SymphonyStateView, error) {
	return e.symphony, e.record("SymphonyState")
}
func (e *extFakeDaemon) SymphonyRefresh(context.Context) error {
	if err := e.record("SymphonyRefresh"); err != nil {
		return err
	}
	e.mu.Lock()
	e.symRefreshes++
	e.mu.Unlock()
	return nil
}
func (e *extFakeDaemon) Notify(_ context.Context, p client.NotifyParams) error {
	if err := e.record("Notify"); err != nil {
		return err
	}
	e.mu.Lock()
	e.notifies = append(e.notifies, p)
	e.mu.Unlock()
	return nil
}
func (e *extFakeDaemon) FollowUp(_ context.Context, p client.FollowUpParams) (client.FollowUpResult, error) {
	if err := e.record("FollowUp"); err != nil {
		return client.FollowUpResult{}, err
	}
	e.mu.Lock()
	e.followUps = append(e.followUps, p)
	e.mu.Unlock()
	id := p.ID
	if id == "" {
		id = p.Task + ".followup.1"
	}
	return client.FollowUpResult{ID: id, PredecessorTask: p.Task, State: "Pending", ResumedSession: "sess-parser-99"}, nil
}
func (e *extFakeDaemon) PoolResize(_ context.Context, p client.PoolResizeParams) error {
	if err := e.record("PoolResize"); err != nil {
		return err
	}
	e.mu.Lock()
	e.poolResizes = append(e.poolResizes, p)
	e.mu.Unlock()
	return nil
}
func (e *extFakeDaemon) QuotaSet(_ context.Context, p client.QuotaSetParams) error {
	if err := e.record("QuotaSet"); err != nil {
		return err
	}
	e.mu.Lock()
	e.quotaSets = append(e.quotaSets, p)
	e.mu.Unlock()
	return nil
}
func (e *extFakeDaemon) QuotaClear(_ context.Context, p client.QuotaClearParams) error {
	if err := e.record("QuotaClear"); err != nil {
		return err
	}
	e.mu.Lock()
	e.quotaClears = append(e.quotaClears, p)
	e.mu.Unlock()
	return nil
}
func (e *extFakeDaemon) DispatchPause(_ context.Context, p client.DispatchPauseParams) error {
	if err := e.record("DispatchPause"); err != nil {
		return err
	}
	e.mu.Lock()
	e.pauseCalls = append(e.pauseCalls, p)
	e.mu.Unlock()
	return nil
}
func (e *extFakeDaemon) DispatchUnpause(_ context.Context, p client.DispatchPauseParams) error {
	if err := e.record("DispatchUnpause"); err != nil {
		return err
	}
	e.mu.Lock()
	e.unpauseCalls = append(e.unpauseCalls, p)
	e.mu.Unlock()
	return nil
}
func (e *extFakeDaemon) Decide(_ context.Context, p client.DecideParams) error {
	if err := e.record("Decide"); err != nil {
		return err
	}
	e.mu.Lock()
	e.decides = append(e.decides, p)
	e.mu.Unlock()
	return nil
}
func (e *extFakeDaemon) ResolveUncertain(_ context.Context, p client.ResolveUncertainParams) error {
	if err := e.record("ResolveUncertain"); err != nil {
		return err
	}
	e.mu.Lock()
	e.resolves = append(e.resolves, p)
	e.mu.Unlock()
	return nil
}
func (e *extFakeDaemon) Lifecycle(_ context.Context, p client.LifecycleParams) error {
	if err := e.record(p.Verb); err != nil {
		return err
	}
	e.mu.Lock()
	e.lifecycles = append(e.lifecycles, p)
	e.mu.Unlock()
	return nil
}

func newExtHarness(t *testing.T, opts Options, d *extFakeDaemon) *harness {
	t.Helper()
	h := harnessFor(t, opts, d)
	h.d = d.fakeDaemon
	h.m.now = func() time.Time { return fixedNow }
	return h
}

func TestStyleHelpersGaugeUntilAndFilter(t *testing.T) {
	if got := renderGauge(2, 4, 6); got != "[███░░░]" {
		t.Fatalf("renderGauge(2, 4, 6) = %q, want [███░░░]", got)
	}
	if got := renderGauge(0, 4, 4); got != "[░░░░]" {
		t.Fatalf("renderGauge(0, 4, 4) = %q, want [░░░░]", got)
	}
	if got := renderGauge(5, 4, 4); got != "[████]" {
		t.Fatalf("renderGauge(5, 4, 4) = %q, want [████]", got)
	}
	if got := until(fixedNow, fixedNow.Add(4*time.Minute+10*time.Second)); got != "in 4m10s" {
		t.Fatalf("until(+4m10s) = %q, want in 4m10s", got)
	}
	if got := until(fixedNow, fixedNow); got != "due" {
		t.Fatalf("until(now) = %q, want due", got)
	}
	if got := until(fixedNow, fixedNow.Add(-time.Minute)); got != "due" {
		t.Fatalf("until(-1m) = %q, want due", got)
	}
	if !matchesFilter("", "parser", "implement") {
		t.Fatal("empty filter must match everything")
	}
	if !matchesFilter("PARS", "parser", "implement") {
		t.Fatal("case-insensitive substring match failed")
	}
	if matchesFilter("nonexistent", "parser", "implement") {
		t.Fatal("unexpected match for nonexistent filter")
	}
}

func TestSevenTabNavigationAndExtendedSnapshotReads(t *testing.T) {
	d := extFixture()
	h := newExtHarness(t, Options{Write: true}, d).start()

	// Tab 4: Stream
	h.press("4")
	h.drain()
	if h.m.view != viewStream {
		t.Fatalf("view = %v, want viewStream", h.m.view)
	}
	if len(d.called("Panes")) == 0 {
		t.Fatal("switching to Stream view did not call Panes")
	}
	h.contains("4 Stream", "pane-1", "parser", "claude-opus-4-6")

	// Tab 5: Pools
	h.press("5")
	h.drain()
	if h.m.view != viewPools {
		t.Fatalf("view = %v, want viewPools", h.m.view)
	}
	for _, method := range []string{"Leases", "ProviderHealth", "DispatchPauses"} {
		if len(d.called(method)) == 0 {
			t.Fatalf("switching to Pools view did not call %s", method)
		}
	}
	h.contains("5 Pools", "claude-main", "acct-primary", "claude-opus-4-6", "investigating flaky test")

	// Tab 6: Symphony
	h.press("6")
	h.drain()
	if h.m.view != viewSymphony {
		t.Fatalf("view = %v, want viewSymphony", h.m.view)
	}
	if len(d.called("SymphonyState")) == 0 {
		t.Fatal("switching to Symphony view did not call SymphonyState")
	}
	h.contains("6 Symphony", "FAE-101", "Parser streaming support", "FAE-102")
	h.press("down")
	h.contains("rate limit exceeded")

	// Tab 7: Security
	h.press("7")
	h.drain()
	if h.m.view != viewSecurity {
		t.Fatalf("view = %v, want viewSecurity", h.m.view)
	}
	for _, method := range []string{"TargetConfigs", "Events"} {
		if len(d.called(method)) == 0 {
			t.Fatalf("switching to Security view did not call %s", method)
		}
	}
	h.contains("7 Security", "target:default-target@r2", "push:ff-only", "scope-widen-requested")
	for range 8 {
		h.press("down")
	}
	h.contains("ext-docs", "/opt/docs")

	// Tab wraps from 7 (viewSecurity) back to 1 (viewMolecules), and shift+tab wraps back to 7.
	h.press("tab")
	h.drain()
	if h.m.view != viewMolecules {
		t.Fatalf("tab from viewSecurity landed on %v, want viewMolecules", h.m.view)
	}
	h.press("shift+tab")
	h.drain()
	if h.m.view != viewSecurity {
		t.Fatalf("shift+tab from viewMolecules landed on %v, want viewSecurity", h.m.view)
	}
}

func TestDetailViewRendersWave19To21FieldsAndMoleculesShowsFanOutBadge(t *testing.T) {
	d := extFixture()
	h := newExtHarness(t, Options{Write: true}, d).start()

	// Open task detail for parser.
	h.press("enter")
	h.drain()

	h.contains(
		"Task parser",
		"Follow-up of: lib",
		"Turn budget: wall 600s, max turns 12, on exhaustion park",
		"FanOut (2 branches)",
		"FanOut stage: implement · join: all · parallel: true",
		"b-lexer  status: Running  stage: code  scope: src/lexer.x  commit: deadbeef",
		"b-ast  status: Done  stage: code  scope: src/ast.x  commit: cafebabe",
		"Turn checkpoints",
		"implement: session sess-parser-99 · tree tree-abc123 · 5 turns · 180s",
		"Read-only files: docs/SPEC.md, api/schema.json",
		"Reads allow: ext-docs/**",
		"Reads deny: secrets/**",
		"Approved widened scope: src/extra.x",
		"Pending request: scope-widen (src/ast.x) — need AST node definitions",
		"Notifications (1)",
		"breaking-change (next-turn): token struct renamed [from lib]",
	)

	// Return to Molecules view: cached detail now shows [FanOut 2 branches] badge on parser.
	h.press("esc")
	h.contains("[FanOut 2 branches]")
}

func TestQuickFilterInMoleculesAndBracketsSubWorkflowNavigation(t *testing.T) {
	d := extFixture()
	parentChild := &client.ChildRef{Stage: "implement", Visit: 1, Task: "parser.sub.1", WorkflowID: "default", Revision: 3}
	childParent := &client.ParentRef{Task: "parser", Stage: "implement", Visit: 1}
	for i := range d.board.Tasks {
		if d.board.Tasks[i].ID == "parser" {
			d.board.Tasks[i].Child = parentChild
		}
	}
	d.board.Tasks = append(d.board.Tasks, client.Task{
		ID: "parser.sub.1", State: "spec", Priority: 20, WorkflowID: "default", WorkflowRevision: 3, MoleculeID: "m1", Version: 1,
		Parent: childParent,
	})
	d.topology.Molecules[0].Stages[1].Tasks = []string{"parser.sub.1"}
	d.details["parser.sub.1"] = client.TaskDetail{
		ID: "parser.sub.1", Version: 1, Title: "Sub task", State: "spec", Stage: "spec",
		Workflow: "default", WorkflowRevision: 3, Molecule: "m1", Parent: childParent, Source: "TaskGet",
	}

	h := newExtHarness(t, Options{Write: true}, d).start()

	// Activate quick filter with '/', filter to "docs", then clear with esc.
	h.press("/")
	h.typeText("docsx")
	h.press("backspace", "enter")
	h.contains("filter: docs", "! docs")
	h.lacks("▶ parser")
	if got := h.m.selectedMoleculeTask(); got != "docs" {
		t.Fatalf("filtered selectedMoleculeTask = %q, want docs", got)
	}
	h.press("esc")
	if h.m.filter != "" {
		t.Fatalf("esc did not clear filter: %q", h.m.filter)
	}
	h.contains("▶ parser")

	// Select parser and use ']' to jump to child, then '[' to jump back to parent.
	h.m.selectTask("parser")
	h.press("]")
	h.drain()
	h.contains("Jumped to child task parser.sub.1")
	if got := h.m.selectedMoleculeTask(); got != "parser.sub.1" {
		t.Fatalf("after ], selected = %q, want parser.sub.1", got)
	}
	h.press("[")
	h.drain()
	h.contains("Jumped to parent task parser")
	if got := h.m.selectedMoleculeTask(); got != "parser" {
		t.Fatalf("after [, selected = %q, want parser", got)
	}

	// Even when selected task has a child, 'C' (Shift+C) opens the Create Task modal.
	h.press("C")
	h.contains("Create a task")
	h.press("esc")
}

func TestCommandPaletteActionMenuAndOperationalModals(t *testing.T) {
	d := extFixture()
	d.inbox.Decisions = []client.InboxTask{{Task: "docs", Stage: "review", Decision: json.RawMessage(`{"question":"Approve docs?"}`)}}
	h := newExtHarness(t, Options{Write: true}, d).start()

	// Command Palette via ':'
	h.press(":")
	h.contains("Command Palette")
	h.typeText("sync-symphony")
	h.press("enter")
	h.drain()
	if d.symRefreshes != 1 {
		t.Fatalf("palette sync-symphony refreshes = %d, want 1", d.symRefreshes)
	}
	h.press("esc")

	// Action menu via 'a'
	h.m.selectTask("parser")
	h.press("a")
	h.contains("Actions for task parser", "Notify (next-turn)", "FollowUp")
	h.press("esc")

	// Notify (next-turn) via 'n'
	h.press("n")
	h.contains("Notify task")
	h.focusField("summary")
	h.typeText("please check edge case")
	h.press("ctrl+s")
	h.drain()
	if len(d.notifies) != 1 || d.notifies[0].Task != "parser" || d.notifies[0].Delivery != "next-turn" || d.notifies[0].Summary != "please check edge case" {
		t.Fatalf("Notify params = %+v", d.notifies)
	}
	h.press("esc")

	// FollowUp via 'f'
	h.press("f")
	h.contains("Create FollowUp task")
	h.focusField("prompt")
	h.typeText("Add benchmarks for parser")
	h.press("ctrl+s")
	h.drain()
	if len(d.followUps) != 1 || d.followUps[0].Task != "parser" || d.followUps[0].Prompt != "Add benchmarks for parser" {
		t.Fatalf("FollowUp params = %+v", d.followUps)
	}
	h.press("esc")

	// Pools shortcuts: 's' (resize), 'q' (quota set), 'p' (pause)
	h.press("5")
	h.drain()
	h.press("s")
	h.contains("Resize provider pool")
	h.focusField("cap")
	h.press("backspace")
	h.typeText("8")
	h.press("ctrl+s")
	h.drain()
	if len(d.poolResizes) != 1 || d.poolResizes[0].Pool != "claude-main" || d.poolResizes[0].Cap != 8 {
		t.Fatalf("PoolResize params = %+v", d.poolResizes)
	}
	h.press("esc")

	// Security shortcuts: 'w' (approve widen) and 'L' (land)
	h.press("7")
	h.drain()
	h.press("w")
	h.contains("Approve scope widen")
	h.press("esc")

	// Attention triage shortcut: 'd' (decide)
	h.press("3")
	h.drain()
	h.press("d")
	h.contains("Decide on task")
	h.press("esc")

	if !reflect.DeepEqual(d.called("Notify", "FollowUp", "PoolResize"), []string{"Notify", "FollowUp", "PoolResize"}) {
		t.Fatalf("unexpected write calls: %v", d.called("Notify", "FollowUp", "PoolResize"))
	}
	if !strings.Contains( strings.Join(d.messages, "\n"), "notify task parser") {
		t.Fatalf("authorizing messages missing notify: %v", d.messages)
	}
}

func TestUnescalateFromDetailMoleculesAndAttentionInOneStep(t *testing.T) {
	d := extFixture()
	h := newExtHarness(t, Options{Write: true}, d).start()

	// 1. Select escalated task "docs" in Molecules and open its Detail view.
	h.m.selectTask("docs")
	h.press("enter")
	h.drain()
	h.contains("Task docs", "Un-escalate", "press I (or u / R / a) then Enter")

	// Pressing 'I' then 'enter' immediately un-escalates via RestartWithInformation with pre-filled context!
	h.press("I")
	h.contains("Un-escalate & Restart task")
	h.press("enter")
	h.drain()
	if len(d.lifecycles) != 1 || d.lifecycles[0].Task != "docs" || d.lifecycles[0].Verb != "RestartWithInformation" || d.lifecycles[0].Context == "" {
		t.Fatalf("expected RestartWithInformation on docs from detail I+enter, got %+v", d.lifecycles)
	}
	h.press("esc")

	// 2. Pressing 'R' inside Detail view of an escalated task also un-escalates instead of toggling retired tasks.
	h.press("R", "enter")
	h.drain()
	if len(d.lifecycles) != 2 || d.lifecycles[1].Task != "docs" || d.lifecycles[1].Verb != "RestartWithInformation" {
		t.Fatalf("expected RestartWithInformation on docs from detail R+enter, got %+v", d.lifecycles)
	}
	h.press("esc")

	// 3. Exit Detail view back to Molecules and press 'u' then 'enter' on escalated task "docs".
	h.press("esc")
	h.press("u", "enter")
	h.drain()
	if len(d.lifecycles) != 3 || d.lifecycles[2].Task != "docs" || d.lifecycles[2].Verb != "RestartWithInformation" {
		t.Fatalf("expected RestartWithInformation on docs from molecules u+enter, got %+v", d.lifecycles)
	}
}

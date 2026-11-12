package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/providerhealth"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
)

func providerPoolWorkflow(id, roleName string, roleCap int, provider, model string) workflow.Document {
	accepted := workflow.Expr{
		Op: "eq",
		Args: []workflow.Expr{
			{Op: "field", Stage: "intake", Path: []string{"accepted"}},
			{Op: "literal", Value: true},
		},
	}
	return currentWorkflowFixture(workflow.Document{
		SchemaVersion: 1,
		ID:            id,
		Start:         "intake",
		Limits:        workflow.Limits{Attempts: 8, TestRuns: 2, ScopeWidens: 1},
		Roles: map[string]workflow.Role{
			roleName: {
				Instructions: "Accept the item.",
				Briefs:       map[string]string{"default": "Item {{task_id}}."},
				Routes:       []workflow.Binding{{Provider: provider, Model: model}},
				Scope:        "workflow-global",
				Cap:          roleCap,
				Outputs:      []string{"IntakeReport"},
			},
		},
		Artifacts: map[string]workflow.ArtifactType{
			"IntakeReport": {
				Version: 1,
				Schema:  json.RawMessage(`{"type":"object","required":["accepted"],"additionalProperties":false,"properties":{"accepted":{"type":"boolean"}}}`),
			},
		},
		Stages: map[string]workflow.Stage{
			"intake":      {Primitive: workflow.AgentTurn, Role: roleName, Output: "IntakeReport"},
			"intake-gate": {Primitive: workflow.Gate, Expr: &accepted, Failure: "stop"},
			"stop":        {Primitive: workflow.Escalate},
		},
		Edges: []workflow.Edge{
			{From: "intake", To: "intake-gate"},
			{From: "intake-gate", To: "Done"},
			{From: "intake-gate", To: "stop"},
		},
	})
}

type noopAdmission struct{}

func (noopAdmission) Acquire(context.Context, engine.Dispatch, execution.Binding, engine.Session) (func() error, error) {
	return func() error { return nil }, nil
}

func awaitActiveDrain(t *testing.T, d *Daemon) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := d.collectDelivered(); err != nil {
			t.Fatal(err)
		}
		d.mu.Lock()
		n := len(d.active)
		d.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("timed out waiting for active turns to drain")
}
func readJobFromDaemon(t *testing.T, d *Daemon, id string) engine.Job {
	t.Helper()
	d.mu.Lock()
	snap, ok := d.jobs[id]
	d.mu.Unlock()
	if !ok {
		t.Fatalf("job %s not found in daemon", id)
	}
	var env envelope
	if err := decodeEnvelope(snap.Payload, &env); err != nil {
		t.Fatalf("decodeEnvelope(%s): %v", id, err)
	}
	return env.Job
}

// FAE-SCH-015 (a) & FAE-SCH-005 (2026-11-05 amendment):
// Two workflows declaring role caps of 4 that both route to the same
// {provider, credential_identity} pool with cap 2 run at most 2 concurrent
// sessions across both workflows. When a session is refused by the provider
// pool, its role lease is released immediately, zero attempts/visits are
// charged, and waiting requests are ordered by FAE-SCH-003 ready-set order.
func TestProviderConcurrencyPoolCrossWorkflowAndReadySetOrder(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)

	a := admissionFixture(t, 8)
	a.daemon = d
	paneFixture(t, a, 60)
	a.inventory = func(context.Context) (map[string]int, error) { return map[string]int{}, nil }

	poolCfg := scheduler.ProviderPoolConfig{
		ID:    "claude-main",
		Match: scheduler.ProviderPoolMatch{Provider: "claude", CredentialIdentity: "default"},
		Cap:   2,
	}
	if err := a.host.ConfigureProviderPools(ctx, []scheduler.ProviderPoolConfig{poolCfg}); err != nil {
		t.Fatal(err)
	}

	wfA := providerPoolWorkflow("wf-alpha", "coder", 4, "claude", "claude-sonnet-4-6")
	wfB := providerPoolWorkflow("wf-beta", "reviewer", 4, "claude", "claude-opus-4-6")

	jobs := map[string]controlplane.JobSnapshot{
		"task-a1": viewJob(t, "task-a1", 20, wfA, 1, "intake"),
		"task-b1": viewJob(t, "task-b1", 20, wfB, 1, "intake"),
		"task-a2": viewJob(t, "task-a2", 10, wfA, 1, "intake"),
	}
	v, err := newScheduleView(jobs)
	if err != nil {
		t.Fatal(err)
	}
	a.view.Store(v)

	now := time.Now().UTC()
	rel1, err := a.Acquire(ctx, agentTurn("task-a1"), execution.Binding{Document: wfA}, engine.Session{
		ID:       "sess-a1",
		TaskID:   "task-a1",
		Role:     "coder",
		Route:    workflow.Binding{Provider: "claude", Model: "claude-sonnet-4-6"},
		Priority: 20,
		Admitted: now.Add(-3 * time.Minute),
	})
	if err != nil {
		t.Fatal("first admission failed:", err)
	}
	defer func() {
		if rel1 != nil {
			_ = rel1()
		}
	}()

	rel2, err := a.Acquire(ctx, agentTurn("task-b1"), execution.Binding{Document: wfB}, engine.Session{
		ID:       "sess-b1",
		TaskID:   "task-b1",
		Role:     "reviewer",
		Route:    workflow.Binding{Provider: "claude", Model: "claude-opus-4-6"},
		Priority: 20,
		Admitted: now.Add(-2 * time.Minute),
	})
	if err != nil {
		t.Fatal("second admission across second workflow failed:", err)
	}
	defer func() {
		if rel2 != nil {
			_ = rel2()
		}
	}()

	// Verify the provider pool is mirrored into controlplane resource_pools.
	mirrored, err := d.store.LeaseSnapshot(ctx, "provider_pool:claude-main")
	if err != nil || len(mirrored.Holders) != 2 || mirrored.Capacity != 2 {
		t.Fatalf("mirrored provider pool = %+v, err = %v", mirrored, err)
	}

	// Third session targeting the same provider pool must fail with errProviderPoolExhausted
	// and release its wf-alpha role slot immediately without holding a heavy unit or pane.
	_, err = a.Acquire(ctx, agentTurn("task-a2"), execution.Binding{Document: wfA}, engine.Session{
		ID:       "sess-a2",
		TaskID:   "task-a2",
		Role:     "coder",
		Route:    workflow.Binding{Provider: "claude", Model: "claude-sonnet-4-6"},
		Priority: 10,
		Admitted: now.Add(-1 * time.Minute),
	})
	var poolExhausted errProviderPoolExhausted
	if !errors.As(err, &poolExhausted) {
		t.Fatalf("expected errProviderPoolExhausted, got %v", err)
	}
	roleKeyA := roleTestKey(wfA, "task-a2", "coder")
	rolePoolA, err := d.store.LeaseSnapshot(ctx, roleKeyA)
	if err != nil || len(rolePoolA.Holders) != 1 || len(rolePoolA.Waiting) != 0 {
		t.Fatalf("role lease was not released on provider pool exhaustion: holders=%d waiting=%d err=%v", len(rolePoolA.Holders), len(rolePoolA.Waiting), err)
	}
	if reason := parkedReason(engine.Job{TaskID: "task-a2", State: "intake", Parked: true, Reason: "infrastructure-deferred"}, a); reason != withheldProviderPool {
		t.Fatalf("expected withheldProviderPool (%q), got %q", withheldProviderPool, reason)
	}

	// Verify FAE-SCH-003 ready-set ordering on the host ledger provider pool when
	// multiple tasks queue (CutOffByQuota first, then Priority desc, Admitted asc, TaskID asc).
	for _, r := range []scheduler.Request{
		{ID: "req-low", TaskID: "task-low", Owner: a.owner, Weight: 1, Priority: 5, Admitted: now.Add(-10 * time.Minute), Queued: now},
		{ID: "req-high-late", TaskID: "task-high-b", Owner: a.owner, Weight: 1, Priority: 50, Admitted: now.Add(-1 * time.Minute), Queued: now},
		{ID: "req-high-early", TaskID: "task-high-a", Owner: a.owner, Weight: 1, Priority: 50, Admitted: now.Add(-5 * time.Minute), Queued: now},
		{ID: "req-cutoff", TaskID: "task-cutoff", Owner: a.owner, Weight: 1, Priority: 1, Admitted: now, CutOffByQuota: true, Queued: now},
	} {
		if err := a.host.EnqueueProviderPool(ctx, "claude-main", r); err != nil {
			t.Fatal(err)
		}
	}
	if err := rel1(); err != nil {
		t.Fatal(err)
	}
	rel1 = nil
	if err := rel2(); err != nil {
		t.Fatal(err)
	}
	rel2 = nil

	liveness := map[scheduler.Owner]scheduler.Liveness{a.owner: scheduler.Alive}
	granted, _, err := a.host.AdvanceProviderPool(ctx, "claude-main", now, liveness)
	if err != nil || len(granted) != 2 {
		t.Fatalf("AdvanceProviderPool granted=%+v err=%v", granted, err)
	}
	if granted[0].ID != "req-cutoff" || granted[1].ID != "req-high-early" {
		t.Fatalf("expected [req-cutoff, req-high-early], got [%s, %s]", granted[0].ID, granted[1].ID)
	}
}

// FAE-SCH-015 (b):
// A Prophet session targeting the same {provider, credential_identity} competes
// for and holds the same provider concurrency pool lease as task sessions.
func TestProviderConcurrencyPoolIncludesProphet(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.ProphetRoutes = []workflow.Binding{{Provider: "claude", Model: "claude-opus-4-6"}}
	cfg.Live.ProviderPools = []scheduler.ProviderPoolConfig{
		{
			ID:    "claude-shared",
			Match: scheduler.ProviderPoolMatch{Provider: "claude", CredentialIdentity: "default"},
			Cap:   1,
		},
	}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) { return hostSample(1, 0.5), nil }

	var holderSeenDuringProphet atomic.Int32
	d.live.runner = prophetRouteRunner(func(ctx context.Context, l agentruntime.Launch) (agentruntime.Collected, error) {
		snaps, err := d.live.guard.host.ProviderPoolsSnapshot(ctx)
		if err == nil {
			if snap, ok := snaps["claude-shared"]; ok {
				holderSeenDuringProphet.Store(int32(len(snap.Pool.Holders)))
			}
		}
		return agentruntime.Collected{ProcessesRetired: true, Output: validProphetReply}, nil
	})

	// Hold the single slot in claude-shared with a task session: Prophet must be refused.
	wf := providerPoolWorkflow("wf-task", "coder", 4, "claude", "claude-sonnet-4-6")
	releaseTask, err := d.live.guard.Acquire(ctx, agentTurn("task-holder"), execution.Binding{Document: wf}, engine.Session{
		ID:       "task-holder-sess",
		TaskID:   "task-holder",
		Role:     "coder",
		Route:    workflow.Binding{Provider: "claude", Model: "claude-sonnet-4-6"},
		Priority: 10,
		Admitted: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.runProphet(ctx, "", "prophet-blocked", json.RawMessage(`[]`)); err == nil {
		t.Fatal("expected Prophet to be blocked when provider pool is at capacity")
	}
	if err := releaseTask(); err != nil {
		t.Fatal(err)
	}

	// Once the task releases the slot, Prophet acquires the provider pool lease, runs, and releases it.
	raw, err := d.runProphet(ctx, "", "prophet-admitted", json.RawMessage(`[]`))
	if err != nil || string(raw) != string(validProphetReply) {
		t.Fatalf("Prophet failed after slot release: err=%v raw=%s", err, raw)
	}
	if got := holderSeenDuringProphet.Load(); got != 1 {
		t.Fatalf("expected Prophet to hold 1 provider pool slot during execution, saw %d", got)
	}
	snaps, err := d.live.guard.host.ProviderPoolsSnapshot(ctx)
	if err != nil || len(snaps["claude-shared"].Pool.Holders) != 0 {
		t.Fatalf("Prophet did not release provider pool slot after completion: %+v %v", snaps["claude-shared"], err)
	}
}

// FAE-SCH-015 (c):
// Lowering cap from 3 to 1 while 3 sessions are in flight blocks new admissions
// until active holders drop below 1 without killing or evicting any in-flight
// session; raising cap takes effect on the next drive round; both write an
// audited AuthorityDecision record.
func TestProviderConcurrencyPoolLiveAuditedResize(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.Live.ProviderPools = []scheduler.ProviderPoolConfig{
		{
			ID:    "claude-resize",
			Match: scheduler.ProviderPoolMatch{Provider: "claude", CredentialIdentity: "default"},
			Cap:   3,
		},
	}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) { return hostSample(1, 0.5), nil }

	wf := providerPoolWorkflow("wf-resize", "coder", 6, "claude", "claude-sonnet-4-6")
	var releases []func() error
	defer func() {
		for _, r := range releases {
			if r != nil {
				_ = r()
			}
		}
	}()
	for i := 1; i <= 3; i++ {
		taskID := fmt.Sprintf("active-%d", i)
		rel, err := d.live.guard.Acquire(ctx, agentTurn(taskID), execution.Binding{Document: wf}, engine.Session{
			ID:       "sess-" + taskID,
			TaskID:   taskID,
			Role:     "coder",
			Route:    workflow.Binding{Provider: "claude", Model: "claude-sonnet-4-6"},
			Priority: 10,
			Admitted: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("acquire %d failed: %v", i, err)
		}
		releases = append(releases, rel)
	}

	// Lower cap from 3 to 1 via PoolResize RPC.
	if _, err := call(t, d, "PoolResize", PoolResizeParams{
		Pool:               "claude-resize",
		Cap:                1,
		Actor:              "operator",
		Reason:             "tighten concurrency during peak window",
		AuthorizingMessage: "message",
	}, ""); err != nil {
		t.Fatal("PoolResize down failed:", err)
	}

	// All 3 active holders remain untouched (not evicted), while AdmissionLimit is 1.
	snaps, err := d.live.guard.host.ProviderPoolsSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap := snaps["claude-resize"]
	if len(snap.Pool.Holders) != 3 || snap.Config.Cap != 1 || snap.Pool.AdmissionLimit != 1 {
		t.Fatalf("after resize down: holders=%d cap=%d admission_limit=%d", len(snap.Pool.Holders), snap.Config.Cap, snap.Pool.AdmissionLimit)
	}

	// Releasing 2 of the 3 active holders leaves 1 holder, which still saturates cap=1.
	for i := 0; i < 2; i++ {
		if err := releases[i](); err != nil {
			t.Fatal(err)
		}
		releases[i] = nil
	}
	_, err = d.live.guard.Acquire(ctx, agentTurn("blocked-at-1"), execution.Binding{Document: wf}, engine.Session{
		ID:       "sess-blocked-at-1",
		TaskID:   "blocked-at-1",
		Role:     "coder",
		Route:    workflow.Binding{Provider: "claude", Model: "claude-sonnet-4-6"},
		Priority: 10,
		Admitted: time.Now().UTC(),
	})
	var poolExhausted errProviderPoolExhausted
	if !errors.As(err, &poolExhausted) {
		t.Fatalf("expected admission to stay blocked with 1 active holder and cap=1, got %v", err)
	}

	// Raise cap from 1 to 2 via PoolResize RPC: new session is immediately admitted!
	if _, err := call(t, d, "PoolResize", PoolResizeParams{
		Pool:               "claude-resize",
		Cap:                2,
		Actor:              "operator",
		Reason:             "restore concurrency",
		AuthorizingMessage: "message",
	}, ""); err != nil {
		t.Fatal("PoolResize up failed:", err)
	}
	relNew, err := d.live.guard.Acquire(ctx, agentTurn("admitted-at-2"), execution.Binding{Document: wf}, engine.Session{
		ID:       "sess-admitted-at-2",
		TaskID:   "admitted-at-2",
		Role:     "coder",
		Route:    workflow.Binding{Provider: "claude", Model: "claude-sonnet-4-6"},
		Priority: 10,
		Admitted: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("expected admission after raising cap to 2, got %v", err)
	}
	releases = append(releases, relNew)

	// Verify AuthorityDecision rows were written for both resize operations.
	var decisions int
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "verify pool resize decisions", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM decisions WHERE kind='pool-resize' AND task_id='pool:claude-resize'`).Scan(&decisions)
	})
	if err != nil || decisions != 2 {
		t.Fatalf("expected 2 pool-resize AuthorityDecision rows, got %d (err=%v)", decisions, err)
	}
}

// FAE-SCH-015 (d):
// A crashed daemon or dead owner holding a provider pool lease has its lease
// reclaimed under FAE-HOST-031 without manual file edits.
func TestProviderConcurrencyPoolDeadOwnerReclamation(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.Live.ProviderPools = []scheduler.ProviderPoolConfig{
		{
			ID:    "claude-reclaim",
			Match: scheduler.ProviderPoolMatch{Provider: "claude", CredentialIdentity: "default"},
			Cap:   1,
		},
	}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	if _, err = call(t, d, "Message", MessageParams{ID: "host", Text: `{"text":"Reclaim host-wide ledgers","authorize":{"classes":["host-wide"]}}`}, ""); err != nil {
		t.Fatal(err)
	}

	dead := deadOwner(t)
	now := time.Now().UTC()
	if err := d.live.guard.host.EnqueueProviderPool(ctx, "claude-reclaim", scheduler.Request{
		ID:     "crashed-provider-turn",
		TaskID: "task-crashed",
		Owner:  dead,
		Weight: 1,
		Queued: now,
	}); err != nil {
		t.Fatal(err)
	}
	if granted, _, err := d.live.guard.host.AdvanceProviderPool(ctx, "claude-reclaim", now, map[scheduler.Owner]scheduler.Liveness{dead: scheduler.Alive}); err != nil || len(granted) != 1 {
		t.Fatalf("granted=%+v err=%v", granted, err)
	}

	var applied reclaimResult
	r, err := call(t, d, "Reclaim", ReclaimParams{HostWide: true, AuthorizingMessage: "host"}, "")
	decodeResult(t, r, err, &applied)
	heavy := applied.ledger("heavy-op")
	if heavy.Status != "reconciled" || len(heavy.Reclaimed) != 1 || heavy.Reclaimed[0].ID != "crashed-provider-turn" || heavy.Reclaimed[0].Pool != "provider_pool:claude-reclaim" {
		t.Fatalf("expected dead provider pool holder to be reclaimed, got %+v", heavy)
	}
	snaps, err := d.live.guard.host.ProviderPoolsSnapshot(ctx)
	if err != nil || len(snaps["claude-reclaim"].Pool.Holders) != 0 {
		t.Fatalf("provider pool still has holders after reclaim: %+v %v", snaps["claude-reclaim"], err)
	}
}

// FAE-RT-018 (a):
// A turn failing with a usage limit reporting reset time T+2h:
// 1. Preserves partial work and session ID on the cut-off task (InterruptedSessionID, CutOffByQuota=true),
// 2. Charges zero attempt, visit, rotation, or recovery budget,
// 3. Parks all 12 tasks on that pool/binding as ProviderQuota,
// 4. Launches no turn before T+grace and never escalates ProviderUnavailable or raises a route-exhaustion alert before T,
// 5. Re-dispatches the cut-off turn first ahead of all other ready tasks once T+grace arrives.
func TestUsageLimitWithResetTimeParks12TasksZeroChargeNoEscalationAndCutOffFirstAtT(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.RouteExhaustion = 30 * time.Minute
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	stopDrive(t, d)

	doc := providerPoolWorkflow("wf-quota-12", "intake", 12, "claude", "claude-sonnet-4-6")
	if _, err := call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}

	baseNow := time.Now().UTC().Truncate(time.Second)
	resetAt := baseNow.Add(2 * time.Hour)
	graceSeconds := 60
	availableAt := resetAt.Add(time.Duration(graceSeconds) * time.Second)

	// Admit 12 tasks: task-01 through task-11 have priority 50; task-12 has priority 10
	// and will be the in-flight turn cut off by the usage limit.
	var taskIDs []string
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("task-%02d", i)
		taskIDs = append(taskIDs, id)
		prio := 50
		if i == 12 {
			prio = 10
		}
		if _, err := call(t, d, "Admit", AdmitParams{
			ID:                 id,
			RunID:              "run-quota-12",
			WorkflowID:         doc.ID,
			Tree:               "tree",
			Priority:           prio,
			AuthorizingMessage: "message",
		}, id); err != nil {
			t.Fatal(id, err)
		}
	}

	// Simulate task-12 executing a turn that gets cut off mid-flight by a usage limit with reset_at = T+2h.
	logDir := t.TempDir()
	rateLimitLine := fmt.Sprintf(`{"type":"system","subtype":"init","session_id":"sess-cutoff-12"}`+"\n"+`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resets_at":%d,"rate_limit_type":"five_hour"}}`+"\n", resetAt.Unix())
	if err := os.WriteFile(filepath.Join(logDir, "events.jsonl"), []byte(rateLimitLine), 0o600); err != nil {
		t.Fatal(err)
	}

	cutoffExec := &execution.Executor{
		Admission: noopAdmission{},
		Runtime: prophetRouteRunner(func(_ context.Context, _ agentruntime.Launch) (agentruntime.Collected, error) {
			return agentruntime.Collected{
				LogDirectory:     logDir,
				ProcessesRetired: true,
			}, errors.New("You've hit your usage limit")
		}),
		Resolve: func(_ context.Context, disp engine.Dispatch) (execution.Binding, error) {
			return execution.Binding{ExecutionHead: disp.Tree, Worktree: t.TempDir(), Document: doc}, nil
		},
		Tree: func(_ context.Context, b execution.Binding) (string, error) {
			return b.ExecutionHead, nil
		},
		ObserveDetail: func(ctx context.Context, disp engine.Dispatch, s engine.Session, detail providerhealth.ObservationDetail) error {
			return d.store.ObserveProvider(ctx, controlplane.ProviderObservation{
				ID:           s.ID,
				TaskID:       disp.TaskID,
				Provider:     s.Route.Provider,
				Model:        s.Route.Model,
				Outcome:      detail.Outcome,
				ResetAt:      detail.ResetAt,
				GraceSeconds: graceSeconds,
				CutOffTaskID: disp.TaskID,
				At:           baseNow,
			})
		},
	}

	d.mu.Lock()
	snap12 := d.jobs["task-12"]
	d.mu.Unlock()
	var env12 envelope
	if err := decodeEnvelope(snap12.Payload, &env12); err != nil {
		t.Fatal(err)
	}

	var prepEvents12 []engine.Event
	var next12 engine.Job
	var disp12 *engine.Dispatch
	if env12.Job.State == domain.Pending {
		var moveEvents []engine.Event
		env12.Job, _, moveEvents, err = engine.PrepareWith(env12.Job, engine.Options{})
		if err != nil {
			t.Fatal("move task-12 from Pending:", err)
		}
		prepEvents12 = append(prepEvents12, moveEvents...)
	}
	var stageEvents []engine.Event
	next12, disp12, stageEvents, err = engine.PrepareWith(env12.Job, engine.Options{})
	if err != nil || disp12 == nil {
		t.Fatalf("PrepareWith task-12: disp=%v err=%v", disp12, err)
	}
	prepEvents12 = append(prepEvents12, stageEvents...)
	execDisp12 := *disp12
	execDisp12.ExecutionKind = "live"
	res12, _ := cutoffExec.Execute(ctx, execDisp12)
	if res12.Class != engine.ClassQuotaCutoff || !res12.QuotaResetAt.Equal(resetAt) {
		t.Fatalf("expected ClassQuotaCutoff with QuotaResetAt=%v, got class=%q reset=%v detail=%q", resetAt, res12.Class, res12.QuotaResetAt, res12.Detail)
	}
	completed12, compEvents12, err := engine.Complete(next12, disp12.ID, res12)
	if err != nil {
		t.Fatal(err)
	}

	// Verify task-12 was refunded (zero attempts, zero stage counts), preserved its InterruptedSessionID
	// and CutOffByQuota flag, and parked as ProviderQuota with RoutesUnavailableSince anchored at T.
	if completed12.Attempts != 0 || completed12.Counts[string(completed12.State)] != 0 || len(completed12.Failures) != 0 {
		t.Fatalf("cut-off turn charged budget: attempts=%d counts=%v failures=%v", completed12.Attempts, completed12.Counts, completed12.Failures)
	}
	if !completed12.Parked || completed12.Reason != engine.ParkProviderQuota || !completed12.CutOffByQuota || completed12.InterruptedSessionID != "sess-cutoff-12" {
		t.Fatalf("unexpected cut-off job12 state: parked=%v reason=%q cutoff=%v session=%q", completed12.Parked, completed12.Reason, completed12.CutOffByQuota, completed12.InterruptedSessionID)
	}
	if completed12.RoutesUnavailableSince.Before(resetAt) {
		t.Fatalf("RoutesUnavailableSince (%v) must be anchored at or after T (%v)", completed12.RoutesUnavailableSince, resetAt)
	}

	// Persist updated job12 back into the daemon & control plane.
	env12.Job = completed12
	allEvents12 := append(prepEvents12, compEvents12...)
	d.mu.Lock()
	snap12, err = d.persist(ctx, snap12, env12, allEvents12)
	if err != nil {
		d.mu.Unlock()
		t.Fatal(err)
	}
	d.jobs["task-12"] = snap12
	d.mu.Unlock()

	// Now run drive rounds while quota-exhausted until T+2h:
	// Round 1 releases task-01..task-11 from Pending to intake;
	// Round 2 parks them as ProviderQuota;
	// Round 3 observes all 12 tasks parked as ProviderQuota in withheld metrics.
	// Zero turns may launch, and all 12 tasks must be parked as ProviderQuota with zero charge.
	var launchedBeforeT atomic.Int32
	origExec := d.executor
	d.executor = executorFunc(func(ctx context.Context, disp engine.Dispatch) (engine.Result, error) {
		launchedBeforeT.Add(1)
		return origExec.Execute(ctx, disp)
	})
	for i := 0; i < 3; i++ {
		if err := d.dispatch(); err != nil {
			t.Fatal(err)
		}
	}
	d.executor = origExec
	if got := launchedBeforeT.Load(); got != 0 {
		t.Fatalf("expected 0 turns launched before T, got %d", got)
	}

	counts, withheldTasks := withheldNow(t, d)
	if counts[withheldProviderQuota] != 12 {
		t.Fatalf("expected 12 tasks withheld as %q, got counts=%v tasks=%v", withheldProviderQuota, counts, withheldTasks)
	}
	for _, id := range taskIDs {
		j := readJobFromDaemon(t, d, id)
		if !j.Parked || j.Reason != engine.ParkProviderQuota || j.Attempts != 0 || j.State == domain.Escalated {
			t.Fatalf("task %s before T: parked=%v reason=%q attempts=%d state=%s", id, j.Parked, j.Reason, j.Attempts, j.State)
		}
	}

	// Advance 45 minutes (exceeding the 30-minute RouteExhaustion bound, but still 1h15m before T):
	// Neither ProviderUnavailable escalation nor route-exhaustion alert may fire before T!
	beforeTPlus45m := baseNow.Add(45 * time.Minute)
	routeExhaustionObserve(t, ctx, d, beforeTPlus45m)
	if alerts := routeExhaustionAlerts(t, d); len(alerts) != 0 {
		t.Fatalf("route-exhaustion alert fired before T: %+v", alerts)
	}
	healthRows, err := d.store.ProviderHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byBinding := map[[2]string]providerhealth.Health{}
	for _, h := range healthRows {
		byBinding[[2]string{h.Provider, h.Model}] = h
	}
	for _, id := range taskIDs {
		j := readJobFromDaemon(t, d, id)
		opts := engine.Options{RouteExhaustion: 30 * time.Minute}
		d.configureLiveRouteOptions(&opts, byBinding, beforeTPlus45m)
		nextJ, disp, _, err := engine.PrepareWith(j, opts)
		if err != nil || disp != nil || nextJ.State == domain.Escalated || nextJ.Reason != engine.ParkProviderQuota {
			t.Fatalf("task %s at T-1h15m: disp=%v state=%s reason=%q err=%v", id, disp, nextJ.State, nextJ.Reason, err)
		}
	}

	// Now verify orderedTaskIDs places the cut-off task ("task-12", priority 10)
	// FIRST ahead of higher-priority tasks ("task-01".."task-11", priority 50)!
	active, err := d.store.ActiveBacklog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	orderAtT, err := d.orderedTaskIDs(controlplane.BacklogGraph{Items: active.Items})
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(orderAtT) != 12 || orderAtT[0] != "task-12" {
		t.Fatalf("expected cut-off task-12 to be ordered first at T, got %v", orderAtT)
	}

	// Verify that at T + grace, the quota window has expired and tasks prepare in that exact order,
	// starting with task-12 first!
	var dispatchedOrder []string
	for _, id := range orderAtT {
		j := readJobFromDaemon(t, d, id)
		j.Parked = false
		j.Reason = ""
		opts := engine.Options{RouteExhaustion: 30 * time.Minute}
		d.configureLiveRouteOptions(&opts, byBinding, availableAt.Add(time.Second))
		_, disp, _, err := engine.PrepareWith(j, opts)
		if err != nil || disp == nil {
			t.Fatalf("task %s did not prepare at T+grace: disp=%v err=%v", id, disp, err)
		}
		dispatchedOrder = append(dispatchedOrder, id)
	}
	if dispatchedOrder[0] != "task-12" || len(dispatchedOrder) != 12 {
		t.Fatalf("expected cut-off task-12 to dispatch first at T+grace, got %v", dispatchedOrder)
	}
}

type executorFunc func(context.Context, engine.Dispatch) (engine.Result, error)

func (f executorFunc) Execute(ctx context.Context, disp engine.Dispatch) (engine.Result, error) {
	return f(ctx, disp)
}

// FAE-RT-018 (b):
// A rate-limit failure without a reset time T spaces probes by exponential
// backoff (1m, 2m, 4m, 8m, ... up to 1h cap).
func TestRateLimitWithoutResetTimeExponentialBackoff(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)

	t0 := time.Now().UTC().Truncate(time.Second)
	for i, wantBackoff := range []time.Duration{1 * time.Minute, 2 * time.Minute, 4 * time.Minute} {
		at := t0.Add(time.Duration(i*10) * time.Minute)
		if err := d.store.ObserveProvider(ctx, controlplane.ProviderObservation{
			ID:       fmt.Sprintf("rl-no-t-%d", i),
			TaskID:   "task-backoff",
			Provider: "claude",
			Model:    "claude-sonnet-4-6",
			Outcome:  providerhealth.RateLimited,
			At:       at,
		}); err != nil {
			t.Fatal(err)
		}
		rows, err := d.store.ProviderHealth(ctx)
		if err != nil || len(rows) != 1 {
			t.Fatalf("step %d: rows=%+v err=%v", i, rows, err)
		}
		h := rows[0]
		wantNextProbe := at.Add(wantBackoff)
		if !h.NextProbeAt.Equal(wantNextProbe) {
			t.Fatalf("step %d: NextProbeAt=%v, want %v", i, h.NextProbeAt, wantNextProbe)
		}
		if h.ProbeDue(at.Add(wantBackoff - time.Second)) {
			t.Fatalf("step %d: route should not be probe-due before NextProbeAt", i)
		}
		if !h.ProbeDue(wantNextProbe) {
			t.Fatalf("step %d: route should be probe-due at NextProbeAt", i)
		}
	}
}

// FAE-RT-018 (c):
// Audited operator QuotaSet and QuotaClear verbs mark or clear quota-exhausted
// until T, record AuthorityDecision rows, and QuotaClear unparks ProviderQuota
// tasks and resumes dispatch in the next drive round.
func TestQuotaSetAndClearAuditedVerbs(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	stopDrive(t, d)

	doc := providerPoolWorkflow("wf-quota-verbs", "intake", 4, "claude", "claude-sonnet-4-6")
	if _, err := call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, d, "Admit", AdmitParams{ID: "quota-verb-task", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "quota-verb-task"); err != nil {
		t.Fatal(err)
	}

	resetAt := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Second)
	if _, err := call(t, d, "QuotaSet", QuotaSetParams{
		Provider:           "claude",
		Model:              "claude-sonnet-4-6",
		ResetAt:            resetAt,
		GraceSeconds:       60,
		Actor:              "operator",
		Reason:             "manual provider quota cap",
		AuthorizingMessage: "message",
	}, ""); err != nil {
		t.Fatal("QuotaSet failed:", err)
	}

	// Round 1 moves quota-verb-task from Pending to intake; Round 2 parks it ProviderQuota.
	if err := d.dispatch(); err != nil {
		t.Fatal(err)
	}
	if err := d.dispatch(); err != nil {
		t.Fatal(err)
	}
	parkedJob := readJobFromDaemon(t, d, "quota-verb-task")
	if !parkedJob.Parked || parkedJob.Reason != engine.ParkProviderQuota || parkedJob.Attempts != 0 {
		t.Fatalf("expected task to park ProviderQuota with 0 attempts after QuotaSet, got parked=%v reason=%q attempts=%d", parkedJob.Parked, parkedJob.Reason, parkedJob.Attempts)
	}

	// Clear quota via QuotaClear and run drive rounds: task must unpark and dispatch!
	if _, err := call(t, d, "QuotaClear", QuotaClearParams{
		Provider:           "claude",
		Model:              "claude-sonnet-4-6",
		Actor:              "operator",
		Reason:             "quota replenished early",
		AuthorizingMessage: "message",
	}, ""); err != nil {
		t.Fatal("QuotaClear failed:", err)
	}
	if err := d.dispatch(); err != nil {
		t.Fatal(err)
	}
	awaitActiveDrain(t, d)
	resumedJob := readJobFromDaemon(t, d, "quota-verb-task")
	if resumedJob.Parked || resumedJob.Attempts == 0 {
		t.Fatalf("expected task to unpark and dispatch on next round after QuotaClear, got state=%s parked=%v reason=%q attempts=%d", resumedJob.State, resumedJob.Parked, resumedJob.Reason, resumedJob.Attempts)
	}

	var setDecisions, clearDecisions int
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "verify quota decisions", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM decisions WHERE kind='quota-set'`).Scan(&setDecisions); e != nil {
			return e
		}
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM decisions WHERE kind='quota-clear'`).Scan(&clearDecisions)
	})
	if err != nil || setDecisions != 1 || clearDecisions != 1 {
		t.Fatalf("expected 1 quota-set and 1 quota-clear decision, got set=%d clear=%d err=%v", setDecisions, clearDecisions, err)
	}
}

// FAE-RT-018 (d):
// A paused task set with 3 turns in flight lets those 3 turns finish cleanly
// without killing them and dispatches nothing more in that set (withheldDispatchPaused,
// zero charge) across a daemon restart; DispatchUnpause resumes dispatch on the
// next round. Also verifies instance, workflow, and pool pause scopes.
func TestDurableScopedDispatchPauseAndUnpauseAcrossRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.MaxInFlight = 6
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
	stopDrive(t, d)

	doc := providerPoolWorkflow("wf-pause", "intake", 6, "claude", "claude-sonnet-4-6")
	if _, err := call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}

	inFlightStarted := make(chan string, 3)
	releaseInFlight := make(chan struct{})
	var inFlightFinished atomic.Int32

	wrapExec := func(target *Daemon) {
		baseExec := target.executor
		target.executor = executorFunc(func(ctx context.Context, disp engine.Dispatch) (engine.Result, error) {
			switch disp.TaskID {
			case "inflight-1", "inflight-2", "inflight-3":
				inFlightStarted <- disp.TaskID
				<-releaseInFlight
				inFlightFinished.Add(1)
			}
			return baseExec.Execute(ctx, disp)
		})
	}
	wrapExec(d)

	for _, id := range []string{"inflight-1", "inflight-2", "inflight-3"} {
		if _, err := call(t, d, "Admit", AdmitParams{
			ID:                 id,
			RunID:              "run-paused",
			WorkflowID:         doc.ID,
			Tree:               "tree",
			AuthorizingMessage: "message",
		}, id); err != nil {
			t.Fatal(id, err)
		}
	}

	// Dispatch the first 3 tasks: Round 1 moves Pending -> intake, Round 2 launches their turns.
	if err := d.dispatch(); err != nil {
		t.Fatal(err)
	}
	if err := d.dispatch(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		select {
		case <-inFlightStarted:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for 3 in-flight turns to start")
		}
	}

	// Admit 2 more tasks into run-paused and apply durable pause to task_set run-paused while 3 turns are in flight.
	for _, id := range []string{"queued-4", "queued-5"} {
		if _, err := call(t, d, "Admit", AdmitParams{
			ID:                 id,
			RunID:              "run-paused",
			WorkflowID:         doc.ID,
			Tree:               "tree",
			AuthorizingMessage: "message",
		}, id); err != nil {
			t.Fatal(id, err)
		}
	}
	if _, err := call(t, d, "DispatchPause", DispatchPauseParams{
		Scope:              "task_set",
		RunID:              "run-paused",
		Actor:              "operator",
		Reason:             "hold queued tasks in run-paused",
		AuthorizingMessage: "message",
	}, ""); err != nil {
		t.Fatal("DispatchPause failed:", err)
	}

	// Let the 3 in-flight turns finish cleanly: they must NOT be killed by DispatchPause.
	close(releaseInFlight)
	awaitActiveDrain(t, d)
	if got := inFlightFinished.Load(); got != 3 {
		t.Fatalf("expected 3 in-flight turns to finish cleanly, got %d", got)
	}

	// Run a drive round: nothing more in run-paused may dispatch!
	if err := d.round(); err != nil {
		t.Fatal(err)
	}
	counts, withheldTasks := withheldNow(t, d)
	if withheldTasks["queued-4"] != withheldDispatchPaused || withheldTasks["queued-5"] != withheldDispatchPaused || counts[withheldDispatchPaused] < 2 {
		t.Fatalf("expected queued-4 and queued-5 withheld as dispatch_paused, got tasks=%v counts=%v", withheldTasks, counts)
	}
	for _, id := range []string{"queued-4", "queued-5"} {
		if j := readJobFromDaemon(t, d, id); j.Attempts != 0 {
			t.Fatalf("paused task %s charged attempt: %d", id, j.Attempts)
		}
	}

	// Restart the daemon: the pause must survive restart and continue withholding queued-4 and queued-5.
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stopDrive(t, d)
	if err := d.dispatch(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"queued-4", "queued-5"} {
		if j := readJobFromDaemon(t, d, id); j.Attempts != 0 {
			t.Fatalf("paused task %s dispatched across restart: state=%s attempts=%d", id, j.State, j.Attempts)
		}
	}

	// Verify workflow and instance scopes also record and list cleanly.
	if _, err := call(t, d, "DispatchPause", DispatchPauseParams{
		Scope:              "workflow",
		WorkflowID:         doc.ID,
		Actor:              "operator",
		Reason:             "pause workflow",
		AuthorizingMessage: "message",
	}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, d, "DispatchUnpause", DispatchPauseParams{
		Scope:              "workflow",
		WorkflowID:         doc.ID,
		Actor:              "operator",
		Reason:             "unpause workflow",
		AuthorizingMessage: "message",
	}, ""); err != nil {
		t.Fatal(err)
	}

	// Now unpause run-paused and run drive rounds (Pending -> Start, then Start -> launch):
	// queued-4 and queued-5 must dispatch!
	if _, err := call(t, d, "DispatchUnpause", DispatchPauseParams{
		Scope:              "task_set",
		RunID:              "run-paused",
		Actor:              "operator",
		Reason:             "resume run-paused",
		AuthorizingMessage: "message",
	}, ""); err != nil {
		t.Fatal("DispatchUnpause failed:", err)
	}
	if err := d.dispatch(); err != nil {
		t.Fatal(err)
	}
	if err := d.dispatch(); err != nil {
		t.Fatal(err)
	}
	awaitActiveDrain(t, d)
	for _, id := range []string{"queued-4", "queued-5"} {
		if j := readJobFromDaemon(t, d, id); j.Attempts == 0 {
			t.Fatalf("expected %s to dispatch on next round after DispatchUnpause, got state=%s attempts=%d", id, j.State, j.Attempts)
		}
	}
}

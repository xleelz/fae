package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/providerhealth"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
)

// TestWave20ObserveProviderBackoffEligibilityAndQuotaClearOnOK verifies Bug 2:
//  1. Store.ObserveProvider initializes h.Outcome and h.RateLimitProbes from the
//     stored row so consecutive rate-limit observations without a reset time
//     follow exponential backoff (1m, 2m, 4m, ...) and Health.Eligible(now)
//     respects NextProbeAt.
//  2. Store.ObserveProvider deletes any active provider-quota marker when
//     observing providerhealth.OK.
func TestWave20ObserveProviderBackoffEligibilityAndQuotaClearOnOK(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)

	t0 := time.Now().UTC().Truncate(time.Second)
	now := t0
	for i, wantDelay := range []time.Duration{1 * time.Minute, 2 * time.Minute, 4 * time.Minute} {
		if err := d.store.ObserveProvider(ctx, controlplane.ProviderObservation{
			ID:       "w20-rl-" + string(rune('a'+i)),
			TaskID:   "task-w20-rl",
			Provider: "claude",
			Model:    "claude-sonnet-4-6",
			Outcome:  providerhealth.RateLimited,
			At:       now,
		}); err != nil {
			t.Fatal(err)
		}
		rows, err := d.store.ProviderHealth(ctx)
		if err != nil || len(rows) != 1 {
			t.Fatalf("step %d: rows=%+v err=%v", i, rows, err)
		}
		h := rows[0]
		probeAt := now.Add(wantDelay)
		if h.Eligible(now) {
			t.Fatalf("step %d: binding must not be eligible immediately after RateLimited at %v", i, now)
		}
		if h.Eligible(probeAt.Add(-time.Second)) {
			t.Fatalf("step %d: binding must not be eligible 1s before NextProbeAt (%v)", i, probeAt)
		}
		if h.Eligible(probeAt) != (h.At(probeAt) >= 50) {
			t.Fatalf("step %d: a due probe must still respect the score threshold, score=%v", i, h.At(probeAt))
		}
		now = probeAt
	}

	// Set a quota reset time T+1h, then observe OK and verify the quota marker is deleted.
	resetAt := now.Add(time.Hour)
	if err := d.store.ObserveProvider(ctx, controlplane.ProviderObservation{
		ID:           "w20-quota-set",
		TaskID:       "task-w20-rl",
		Provider:     "claude",
		Model:        "claude-sonnet-4-6",
		Outcome:      providerhealth.RateLimited,
		At:           now,
		ResetAt:      resetAt,
		GraceSeconds: 60,
		CutOffTaskID: "task-w20-rl",
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := d.store.ProviderHealth(ctx)
	if err != nil || len(rows) != 1 || !rows[0].QuotaActive(now.Add(time.Minute)) {
		t.Fatalf("expected active quota after rate-limit with resetAt, got %+v err=%v", rows, err)
	}

	okAt := resetAt.Add(2 * time.Minute)
	if err := d.store.ObserveProvider(ctx, controlplane.ProviderObservation{
		ID:       "w20-quota-ok",
		TaskID:   "task-w20-rl",
		Provider: "claude",
		Model:    "claude-sonnet-4-6",
		Outcome:  providerhealth.OK,
		At:       okAt,
	}); err != nil {
		t.Fatal(err)
	}
	markers, err := d.store.MarkersWithPrefix(ctx, "provider-quota/", 10)
	if err != nil || len(markers) != 0 {
		t.Fatalf("expected provider-quota marker deleted on OK, got %v (err=%v)", markers, err)
	}
}

// TestWave20ConcurrentAcquirePreservesReadySetOrderAndWithheldReasons verifies Bugs 3 & 4:
//  1. Concurrent Acquire calls for tasks with role caps enter TryAcquireProviderPool
//     without Dolt SQL commit staggering, so CutOffByQuota wins first, followed by
//     higher Priority and earlier Admitted timestamp (FAE-SCH-003).
//  2. Refusal reasons preserve withheldDispatchPaused, withheldProviderQuota, and
//     withheldProviderPool.
func TestWave20ConcurrentAcquirePreservesReadySetOrderAndWithheldReasons(t *testing.T) {
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
		ID:    "claude-order-pool",
		Match: scheduler.ProviderPoolMatch{Provider: "claude", CredentialIdentity: "sub-order"},
		Cap:   1,
	}
	if err := a.host.ConfigureProviderPools(ctx, []scheduler.ProviderPoolConfig{poolCfg}); err != nil {
		t.Fatal(err)
	}

	wf := providerPoolWorkflow("wf-w20-order", "coder", 8, "claude", "claude-sonnet-4-6")
	jobs := map[string]controlplane.JobSnapshot{
		"task-low":    viewJob(t, "task-low", 10, wf, 1, "intake"),
		"task-high-b": viewJob(t, "task-high-b", 50, wf, 1, "intake"),
		"task-high-a": viewJob(t, "task-high-a", 50, wf, 1, "intake"),
		"task-cutoff": viewJob(t, "task-cutoff", 5, wf, 1, "intake"),
	}
	v, err := newScheduleView(jobs)
	if err != nil {
		t.Fatal(err)
	}
	a.view.Store(v)

	now := time.Now().UTC()

	// Verify withheldDispatchPaused when pool is paused.
	if err := a.host.SetProviderPoolPause(ctx, scheduler.ProviderPoolPause{
		PoolID: "claude-order-pool",
		Paused: true,
		Actor:  "operator",
		Reason: "maintenance pause",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = a.Acquire(ctx, agentTurn("task-low"), execution.Binding{Document: wf}, engine.Session{
		ID:                 "sess-paused",
		Role:               "coder",
		Route:              workflow.Binding{Provider: "claude", Model: "claude-sonnet-4-6"},
		CredentialIdentity: "sub-order",
		Admitted:           now,
	})
	if err == nil {
		t.Fatal("expected paused pool to refuse admission")
	}
	if got := a.refusalReason("task-low"); got != withheldDispatchPaused {
		t.Fatalf("refusalReason for paused pool = %q, want %q", got, withheldDispatchPaused)
	}
	if err := a.host.SetProviderPoolPause(ctx, scheduler.ProviderPoolPause{
		PoolID: "claude-order-pool",
		Paused: false,
		Actor:  "operator",
		Reason: "resume",
	}); err != nil {
		t.Fatal(err)
	}

	// Verify withheldProviderQuota when pool is quota-exhausted.
	if err := a.host.SetProviderPoolQuota(ctx, scheduler.ProviderPoolQuota{
		PoolID:         "claude-order-pool",
		ResetAt:        now.Add(time.Hour),
		GraceSeconds:   60,
		ExhaustedUntil: now.Add(time.Hour + time.Minute),
		Reason:         "quota hit",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = a.Acquire(ctx, agentTurn("task-low"), execution.Binding{Document: wf}, engine.Session{
		ID:                 "sess-quota",
		Role:               "coder",
		Route:              workflow.Binding{Provider: "claude", Model: "claude-sonnet-4-6"},
		CredentialIdentity: "sub-order",
		Admitted:           now,
	})
	if err == nil {
		t.Fatal("expected quota-exhausted pool to refuse admission")
	}
	if got := a.refusalReason("task-low"); got != withheldProviderQuota {
		t.Fatalf("refusalReason for quota-exhausted pool = %q, want %q", got, withheldProviderQuota)
	}
	if err := a.host.ClearProviderPoolQuota(ctx, "claude-order-pool", "operator", "clear"); err != nil {
		t.Fatal(err)
	}

	// Now launch 4 concurrent Acquire calls at the exact same instant on Cap=1 pool:
	// task-cutoff (CutOffByQuota=true, prio=5) must win over task-high-a (prio=50),
	// task-high-b (prio=50), and task-low (prio=10).
	type attempt struct {
		taskID  string
		sess    engine.Session
		release func() error
		err     error
	}
	attempts := []attempt{
		{
			taskID: "task-low",
			sess: engine.Session{
				ID:                 "sess-order-low",
				Role:               "coder",
				Route:              workflow.Binding{Provider: "claude", Model: "claude-sonnet-4-6"},
				CredentialIdentity: "sub-order",
				Priority:           10,
				Admitted:           now.Add(-10 * time.Minute),
			},
		},
		{
			taskID: "task-high-b",
			sess: engine.Session{
				ID:                 "sess-order-high-b",
				Role:               "coder",
				Route:              workflow.Binding{Provider: "claude", Model: "claude-sonnet-4-6"},
				CredentialIdentity: "sub-order",
				Priority:           50,
				Admitted:           now.Add(-1 * time.Minute),
			},
		},
		{
			taskID: "task-high-a",
			sess: engine.Session{
				ID:                 "sess-order-high-a",
				Role:               "coder",
				Route:              workflow.Binding{Provider: "claude", Model: "claude-sonnet-4-6"},
				CredentialIdentity: "sub-order",
				Priority:           50,
				Admitted:           now.Add(-5 * time.Minute),
			},
		},
		{
			taskID: "task-cutoff",
			sess: engine.Session{
				ID:                 "sess-order-cutoff",
				Role:               "coder",
				Route:              workflow.Binding{Provider: "claude", Model: "claude-sonnet-4-6"},
				CredentialIdentity: "sub-order",
				Priority:           5,
				Admitted:           now,
				CutOffByQuota:      true,
			},
		},
	}

	startGate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range attempts {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startGate
			rel, e := a.Acquire(ctx, agentTurn(attempts[i].taskID), execution.Binding{Document: wf}, attempts[i].sess)
			attempts[i].release = rel
			attempts[i].err = e
		}()
	}
	close(startGate)
	wg.Wait()

	defer func() {
		for _, att := range attempts {
			if att.release != nil {
				_ = att.release()
			}
		}
	}()

	for _, att := range attempts {
		if att.taskID == "task-cutoff" {
			if att.err != nil {
				t.Fatalf("expected CutOffByQuota task-cutoff to win provider pool admission, got err=%v", att.err)
			}
		} else {
			if att.err == nil {
				t.Fatalf("expected %s to be refused while task-cutoff took Cap=1 slot", att.taskID)
			}
			if got := a.refusalReason(att.taskID); got != withheldProviderPool {
				t.Fatalf("refusalReason(%s) = %q, want %q", att.taskID, got, withheldProviderPool)
			}
		}
	}
}

// TestWave20LeasesStatusProviderPoolsAndProphetCredential verifies Bugs 5 & 6:
//  1. Prophet sessions populate CredentialIdentity via credentialIdentityForRoute.
//  2. PoolResize supports Cap=-1 (unbounded) and QuotaSet/QuotaClear mirror the
//     provider pool into controlplane resource_pools.
//  3. Leases RPC and Status / sampleStatusExtras report provider_pools,
//     dispatch_pauses, and LeaseUsageByKind reflects AdmissionLimit.
func TestWave20LeasesStatusProviderPoolsAndProphetCredential(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.ProphetRoutes = []workflow.Binding{{Provider: "claude", Model: "claude-opus-4-6"}}
	cfg.Live.Environment = map[string]string{"FAE_CREDENTIAL_CLAUDE": "cred-prophet-sub"}
	cfg.Live.ProviderPools = []scheduler.ProviderPoolConfig{
		{
			ID:    "claude-prophet-sub",
			Match: scheduler.ProviderPoolMatch{Provider: "claude", CredentialIdentity: "cred-prophet-sub"},
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

	if got := d.credentialIdentityForRoute(workflow.Binding{Provider: "claude", Model: "claude-opus-4-6"}); got != "cred-prophet-sub" {
		t.Fatalf("credentialIdentityForRoute = %q, want cred-prophet-sub", got)
	}

	var seenHolders int
	d.live.runner = prophetRouteRunner(func(ctx context.Context, _ agentruntime.Launch) (agentruntime.Collected, error) {
		snaps, e := d.live.guard.host.ProviderPoolsSnapshot(ctx)
		if e == nil {
			seenHolders = len(snaps["claude-prophet-sub"].Pool.Holders)
		}
		return agentruntime.Collected{ProcessesRetired: true, Output: validProphetReply}, nil
	})
	if _, err := d.runProphet(ctx, "", "prophet-cred-check", json.RawMessage(`[]`)); err != nil {
		t.Fatalf("runProphet failed: %v", err)
	}
	if seenHolders != 1 {
		t.Fatalf("expected Prophet to match claude-prophet-sub pool (holders=1), saw %d", seenHolders)
	}

	// Resize claude-prophet-sub down from 3 to 1 and verify LeaseUsageByKind reports capacity=1 (AdmissionLimit).
	if _, err := call(t, d, "PoolResize", PoolResizeParams{
		Pool:               "claude-prophet-sub",
		Cap:                1,
		Actor:              "operator",
		Reason:             "lower cap to 1",
		AuthorizingMessage: "message",
	}, ""); err != nil {
		t.Fatal("PoolResize to 1 failed:", err)
	}
	usage, err := d.store.LeaseUsageByKind(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if usage["provider_pool"].Capacity != 1 {
		t.Fatalf("LeaseUsageByKind[provider_pool].Capacity = %d, want 1 (AdmissionLimit)", usage["provider_pool"].Capacity)
	}

	// Resize claude-prophet-sub to -1 (unbounded) and verify no error mismatch.
	if _, err := call(t, d, "PoolResize", PoolResizeParams{
		Pool:               "claude-prophet-sub",
		Cap:                -1,
		Actor:              "operator",
		Reason:             "make pool unbounded",
		AuthorizingMessage: "message",
	}, ""); err != nil {
		t.Fatal("PoolResize to -1 (unbounded) failed:", err)
	}

	// QuotaSet and QuotaClear on the pool, plus a DispatchPause.
	resetAt := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	if _, err := call(t, d, "QuotaSet", QuotaSetParams{
		Pool:               "claude-prophet-sub",
		ResetAt:            resetAt,
		GraceSeconds:       60,
		Actor:              "operator",
		Reason:             "test pool quota",
		AuthorizingMessage: "message",
	}, ""); err != nil {
		t.Fatal("QuotaSet on pool failed:", err)
	}
	if _, err := call(t, d, "DispatchPause", DispatchPauseParams{
		Scope:              "pool",
		Pool:               "claude-prophet-sub",
		Actor:              "operator",
		Reason:             "pause pool for inspection",
		AuthorizingMessage: "message",
	}, ""); err != nil {
		t.Fatal("DispatchPause on pool failed:", err)
	}

	// Verify Leases RPC includes provider_pools.
	leasesResp, err := call(t, d, "Leases", map[string]any{}, "")
	if err != nil {
		t.Fatal("Leases RPC failed:", err)
	}
	var leasesBody map[string]any
	if err := json.Unmarshal(leasesResp.Result, &leasesBody); err != nil {
		t.Fatal(err)
	}
	ppMap, ok := leasesBody["provider_pools"].(map[string]any)
	if !ok || ppMap["claude-prophet-sub"] == nil {
		t.Fatalf("Leases missing provider_pools[claude-prophet-sub]: %v", leasesBody["provider_pools"])
	}

	// Verify sampleStatusExtras / statusExtraFields includes provider_pools and dispatch_pauses.
	extras := d.sampleStatusExtras(ctx, func(context.Context) (hostguard.Sample, error) {
		return hostSample(1, 0.5), nil
	})
	d.statusExtras.value.Store(extras)
	fields := d.statusExtraFields()
	if fields["provider_pools"] == nil {
		t.Fatalf("statusExtraFields missing provider_pools: %+v", fields)
	}
	pauses, ok := fields["dispatch_pauses"].([]controlplane.DispatchPause)
	if !ok || len(pauses) != 1 || pauses[0].Target != "claude-prophet-sub" {
		t.Fatalf("statusExtraFields unexpected dispatch_pauses: %+v", fields["dispatch_pauses"])
	}

	if _, err := call(t, d, "QuotaClear", QuotaClearParams{
		Pool:               "claude-prophet-sub",
		Actor:              "operator",
		Reason:             "clear pool quota",
		AuthorizingMessage: "message",
	}, ""); err != nil {
		t.Fatal("QuotaClear on pool failed:", err)
	}
}

package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestProviderPoolsReadySetOrderAndCap(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := OpenDynamicLedger(filepath.Join(dir, "ledger"), 2)
	if err != nil {
		t.Fatal(err)
	}
	err = l.ConfigureProviderPools(ctx, []ProviderPoolConfig{
		{
			ID:    "anthropic-sub-a",
			Match: ProviderPoolMatch{Provider: "anthropic", CredentialIdentity: "sub-a"},
			Cap:   1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	snap, matched, err := l.MatchProviderPool(ctx, "anthropic", "sub-a")
	if err != nil || !matched || snap.Config.ID != "anthropic-sub-a" || snap.Config.Cap != 1 {
		t.Fatalf("unexpected match: snap=%+v matched=%v err=%v", snap, matched, err)
	}

	owner, err := CurrentOwner()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	// Enqueue 4 requests out of order:
	// 1. low-prio admitted earlier
	// 2. high-prio admitted later
	// 3. high-prio admitted earlier
	// 4. cut-off-by-quota task (even with lower priority, must go first per FAE-RT-018)
	reqs := []Request{
		{ID: "req-low", Owner: owner, TaskID: "task-low", Weight: 1, Priority: 10, Admitted: now.Add(-10 * time.Minute), Queued: now},
		{ID: "req-high-late", Owner: owner, TaskID: "task-high-late", Weight: 1, Priority: 50, Admitted: now.Add(-1 * time.Minute), Queued: now},
		{ID: "req-high-early", Owner: owner, TaskID: "task-high-early", Weight: 1, Priority: 50, Admitted: now.Add(-5 * time.Minute), Queued: now},
		{ID: "req-cutoff", Owner: owner, TaskID: "task-cutoff", Weight: 1, Priority: 5, Admitted: now, CutOffByQuota: true, Queued: now},
	}
	for _, r := range reqs {
		if err := l.EnqueueProviderPool(ctx, "anthropic-sub-a", r); err != nil {
			t.Fatal(err)
		}
	}

	wantOrder := []string{"req-cutoff", "req-high-early", "req-high-late", "req-low"}
	for _, wantID := range wantOrder {
		granted, _, err := l.AdvanceProviderPool(ctx, "anthropic-sub-a", now, map[Owner]Liveness{owner: Alive})
		if err != nil {
			t.Fatal(err)
		}
		if len(granted) != 1 || granted[0].ID != wantID {
			t.Fatalf("granted %+v, want %s", granted, wantID)
		}
		if err := l.ReleaseProviderPool(ctx, "anthropic-sub-a", wantID, owner, "done"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProviderPoolResizeDownAndUp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := OpenDynamicLedger(filepath.Join(dir, "ledger"), 2)
	if err != nil {
		t.Fatal(err)
	}
	err = l.ConfigureProviderPools(ctx, []ProviderPoolConfig{
		{
			ID:    "pool-a",
			Match: ProviderPoolMatch{Provider: "anthropic", CredentialIdentity: "default"},
			Cap:   2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := CurrentOwner()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	for _, id := range []string{"h1", "h2", "w1"} {
		if err := l.EnqueueProviderPool(ctx, "pool-a", Request{ID: id, Owner: owner, TaskID: id, Weight: 1, Priority: 10, Admitted: now, Queued: now}); err != nil {
			t.Fatal(err)
		}
	}
	granted, _, err := l.AdvanceProviderPool(ctx, "pool-a", now, map[Owner]Liveness{owner: Alive})
	if err != nil || len(granted) != 2 {
		t.Fatalf("expected 2 granted, got %+v (err=%v)", granted, err)
	}

	// Lower cap from 2 to 1: active holders h1 and h2 are NOT evicted, but w1 cannot be admitted.
	snap, err := l.ResizeProviderPool(ctx, ProviderPoolConfig{ID: "pool-a", Cap: 1}, "lower cap")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Pool.Holders) != 2 || snap.Pool.AdmissionLimit != 1 {
		t.Fatalf("expected 2 holders and admission_limit=1 after resize down, got %+v", snap.Pool)
	}

	// Releasing h1 drops active holders to 1, which is still at the new cap of 1, so w1 is still blocked.
	if err := l.ReleaseProviderPool(ctx, "pool-a", "h1", owner, "done"); err != nil {
		t.Fatal(err)
	}
	granted, _, err = l.AdvanceProviderPool(ctx, "pool-a", now, map[Owner]Liveness{owner: Alive})
	if err != nil || len(granted) != 0 {
		t.Fatalf("expected 0 granted while 1 holder remains at cap=1, got %+v", granted)
	}

	// Raising cap from 1 to 2 immediately grants w1 during ResizeProviderPool.
	snap, err = l.ResizeProviderPool(ctx, ProviderPoolConfig{ID: "pool-a", Cap: 2}, "raise cap")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Pool.Holders) != 2 || len(snap.Pool.Waiting) != 0 {
		t.Fatalf("expected w1 granted on resize up to 2, got holders=%+v waiting=%+v", snap.Pool.Holders, snap.Pool.Waiting)
	}
}

func TestProviderPoolCrashRecoveryAndDeadOwnerReclamation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := OpenDynamicLedger(filepath.Join(dir, "ledger"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.ConfigureProviderPools(ctx, []ProviderPoolConfig{
		{
			ID:    "pool-recover",
			Match: ProviderPoolMatch{Provider: "openai", CredentialIdentity: "default"},
			Cap:   1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	deadOwner := Owner{PID: 999999, Started: 12345}
	liveOwner, err := CurrentOwner()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := l.EnqueueProviderPool(ctx, "pool-recover", Request{ID: "dead-lease", Owner: deadOwner, TaskID: "t-dead", Weight: 1, Queued: now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.AdvanceProviderPool(ctx, "pool-recover", now, map[Owner]Liveness{deadOwner: Alive}); err != nil {
		t.Fatal(err)
	}

	// ReclaimDead with dead owner proof reclaims the provider pool lease.
	report, err := l.ReclaimDead(ctx, func(o Owner) Liveness {
		if o == deadOwner {
			return Dead
		}
		return Alive
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Reclaimed) != 1 || report.Reclaimed[0].ID != "dead-lease" || report.Reclaimed[0].Pool != "provider_pool:pool-recover" {
		t.Fatalf("unexpected reclaim report: %+v", report)
	}

	// Also verify OperationProof recovery on ProviderPool.
	if err := l.EnqueueProviderPool(ctx, "pool-recover", Request{ID: "proof-lease", Owner: liveOwner, TaskID: "t-proof", Weight: 1, Queued: now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.AdvanceProviderPool(ctx, "pool-recover", now, map[Owner]Liveness{liveOwner: Alive}); err != nil {
		t.Fatal(err)
	}
	if err := l.Recover(ctx, OperationProof{ID: "proof-lease", Session: "proof-lease", Instance: "inst-1", Birth: "birth-1", Owner: liveOwner, State: "retired", ProviderPool: "pool-recover"}, true); err != nil {
		t.Fatal(err)
	}
	snaps, err := l.ProviderPoolsSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps["pool-recover"].Pool.Holders) != 0 {
		t.Fatalf("expected 0 holders after proof recovery, got %+v", snaps["pool-recover"].Pool.Holders)
	}
}

func TestProviderPoolConcurrentTryAcquireReadySetCoalescingAndUnboundedResize(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := OpenDynamicLedger(filepath.Join(dir, "ledger"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.ConfigureProviderPools(ctx, []ProviderPoolConfig{
		{
			ID:    "pool-coalesce",
			Match: ProviderPoolMatch{Provider: "claude", CredentialIdentity: "sub-1"},
			Cap:   1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := CurrentOwner()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	type candidate struct {
		req     Request
		granted bool
		err     error
	}
	cands := []candidate{
		{req: Request{ID: "c-low", Owner: owner, TaskID: "task-low", Weight: 1, Priority: 10, Admitted: now.Add(-10 * time.Minute), Queued: now}},
		{req: Request{ID: "c-high", Owner: owner, TaskID: "task-high", Weight: 1, Priority: 80, Admitted: now.Add(-5 * time.Minute), Queued: now}},
		{req: Request{ID: "c-cutoff", Owner: owner, TaskID: "task-cutoff", Weight: 1, Priority: 5, Admitted: now, CutOffByQuota: true, Queued: now}},
	}
	done := make(chan int, len(cands))
	for i := range cands {
		i := i
		go func() {
			_, ok, e := l.TryAcquireProviderPool(ctx, "pool-coalesce", cands[i].req, 30*time.Millisecond)
			cands[i].granted = ok
			cands[i].err = e
			done <- i
		}()
	}
	for range cands {
		<-done
	}
	for _, c := range cands {
		if c.err != nil {
			t.Fatalf("%s error: %v", c.req.ID, c.err)
		}
		want := c.req.ID == "c-cutoff"
		if c.granted != want {
			t.Fatalf("%s granted=%v, want %v", c.req.ID, c.granted, want)
		}
	}
	if err := l.ReleaseProviderPool(ctx, "pool-coalesce", "c-cutoff", owner, "done"); err != nil {
		t.Fatal(err)
	}

	// Verify Cap == -1 (unbounded) is accepted by ValidateProviderPoolConfig and ResizeProviderPool.
	if err := ValidateProviderPoolConfig(ProviderPoolConfig{
		ID:    "pool-coalesce",
		Match: ProviderPoolMatch{Provider: "claude", CredentialIdentity: "sub-1"},
		Cap:   -1,
	}); err != nil {
		t.Fatalf("ValidateProviderPoolConfig rejected Cap=-1: %v", err)
	}
	snap, err := l.ResizeProviderPool(ctx, ProviderPoolConfig{ID: "pool-coalesce", Cap: -1}, "remove cap")
	if err != nil {
		t.Fatalf("ResizeProviderPool rejected Cap=-1: %v", err)
	}
	if snap.Config.Cap != -1 || snap.Pool.AdmissionLimit != UnboundedProviderPoolCap {
		t.Fatalf("unexpected unbounded snapshot: %+v", snap)
	}
}

package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/scheduler"
)

func poolColumn(t *testing.T, s *Store, key string) string {
	t.Helper()
	var text, kind string
	if e := s.db.QueryRowContext(context.Background(), `SELECT payload FROM resource_pools WHERE pool_key=?`, key).Scan(&text); e != nil {
		t.Fatal(e)
	}
	if e := s.db.QueryRowContext(context.Background(), `SELECT data_type FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='resource_pools' AND column_name='payload'`).Scan(&kind); e != nil || strings.ToLower(kind) != "text" {
		t.Fatalf("resource_pools.payload is %q, not a bounded TEXT column: %v", kind, e)
	}
	return text
}

func roleWaiter(i int, now time.Time) scheduler.Request {
	return scheduler.Request{ID: fmt.Sprintf("task-%04d", i), Owner: scheduler.Owner{PID: 1000 + i, Started: 1}, Weight: 1, Queued: now.Add(time.Duration(i) * time.Millisecond), Unbounded: true}
}

// FAE-CP-012: no row column exceeds the configured inline limit. Role-pool
// waiters grow the lease ledger's row without bound, so a pool past the
// limit is stored as a content-addressed blob; the column holds the
// reference and every reader and writer of the ledger resolves it.
func TestLeasePoolPayloadSpillsPastConfiguredLimit(t *testing.T) {
	s := New(testPlane(t, SchemaVersion).db, Options{InlineLimit: MinInlineLimit})
	ctx := context.Background()
	a := Audit{Actor: "daemon", Action: "role waiter", Outcome: "ok"}
	if e := s.EnsureLeasePool(ctx, a, LeasePoolConfig{Key: "role:coder", Kind: "role", Capacity: 1}); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	waiters := 0
	for ; waiters < 400 && !strings.HasPrefix(poolColumn(t, s, "role:coder"), `{"blob_ref":`); waiters++ {
		if e := s.EnqueueLease(ctx, a, "role:coder", roleWaiter(waiters, now)); e != nil {
			t.Fatal(e)
		}
		if n := len(poolColumn(t, s, "role:coder")); n > MinInlineLimit {
			t.Fatalf("pool row holds %d bytes inline past the %d limit", n, MinInlineLimit)
		}
	}
	for end := waiters + 5; waiters < end; waiters++ {
		if e := s.EnqueueLease(ctx, a, "role:coder", roleWaiter(waiters, now)); e != nil {
			t.Fatal(e)
		}
	}
	if text := poolColumn(t, s, "role:coder"); !strings.HasPrefix(text, `{"blob_ref":`) || len(text) > MinInlineLimit {
		t.Fatalf("%d waiters left the pool inline: %d bytes", waiters, len(text))
	}
	pool, e := s.LeaseSnapshot(ctx, "role:coder")
	if e != nil || len(pool.Waiting) != waiters {
		t.Fatalf("snapshot %d of %d waiters: %v", len(pool.Waiting), waiters, e)
	}
	update, e := s.AdvanceLeases(ctx, a, "role:coder", now.Add(time.Second), map[scheduler.Owner]scheduler.Liveness{})
	if e != nil || len(update.Granted) != 1 || update.Granted[0].ID != "task-0000" {
		t.Fatalf("grant from a spilled pool %+v %v", update.Granted, e)
	}
	if e = s.CancelLease(ctx, a, "role:coder", "task-0001", roleWaiter(1, now).Owner, "cancelled"); e != nil {
		t.Fatal(e)
	}
	pools, e := s.LeasePools(ctx)
	if e != nil || len(pools) != 1 || len(pools[0].Pool.Holders) != 1 || len(pools[0].Pool.Waiting) != waiters-2 {
		t.Fatalf("lease pools %+v %v", pools, e)
	}
	usage, e := s.LeaseUsageByKind(ctx)
	if e != nil || usage["role"].Holders != 1 || usage["role"].Waiters != waiters-2 {
		t.Fatalf("lease usage %+v %v", usage, e)
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM leases WHERE scope_key='role:coder' AND released_at IS NULL`); n != 1 {
		t.Fatalf("projected holders %d", n)
	}
	// A shrinking pool returns inline.
	for i := 2; i < waiters; i++ {
		if e = s.CancelLease(ctx, a, "role:coder", roleWaiter(i, now).ID, roleWaiter(i, now).Owner, "cancelled"); e != nil {
			t.Fatal(e)
		}
	}
	if text := poolColumn(t, s, "role:coder"); strings.HasPrefix(text, `{"blob_ref":`) {
		t.Fatal("an emptied pool stayed a reference")
	}
}

// Migration 10 stores a pool that outgrew the limit before it by reference
// and bounds the column; the step is idempotent, since Dolt DDL may commit
// implicitly (FAE-CP-010).
func TestMigration10SpillsOversizedPoolsAndBoundsColumn(t *testing.T) {
	s := testPlane(t, 9)
	ctx := context.Background()
	a := Audit{Actor: "daemon", Action: "pre-10", Outcome: "ok"}
	if e := s.EnsureLeasePool(ctx, a, LeasePoolConfig{Key: "role:big", Kind: "role", Capacity: 1}); e != nil {
		t.Fatal(e)
	}
	pool, _ := scheduler.NewPool(1)
	now := time.Now().UTC()
	for i := 0; i < 600; i++ {
		pool.Waiting = append(pool.Waiting, roleWaiter(i, now))
	}
	raw, _ := json.Marshal(pool)
	if len(raw) <= DefaultInlineLimit {
		t.Fatalf("fixture pool is only %d bytes", len(raw))
	}
	if _, e := s.db.ExecContext(ctx, `UPDATE resource_pools SET payload=? WHERE pool_key='role:big'`, string(raw)); e != nil {
		t.Fatal(e)
	}
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	for range 2 {
		if text := poolColumn(t, s, "role:big"); !strings.HasPrefix(text, `{"blob_ref":`) {
			t.Fatalf("migrated pool stayed inline: %d bytes", len(text))
		}
		if got, e := s.LeaseSnapshot(ctx, "role:big"); e != nil || len(got.Waiting) != 600 {
			t.Fatalf("migrated snapshot %d waiters: %v", len(got.Waiting), e)
		}
		if e := s.transaction(ctx, func(ctx context.Context, tx *sql.Tx) error { return s.migration10(ctx, tx) }); e != nil {
			t.Fatal("migration 10 replay", e)
		}
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM migrations WHERE version=10`); n != 1 {
		t.Fatal("migration 10 recorded", n)
	}
}

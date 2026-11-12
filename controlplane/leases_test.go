package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/xleelz/fae/scheduler"
	"sync"
	"testing"
	"time"
)

func TestLeaseValidationBeforeSQL(t *testing.T) {
	s, _ := fixture(t, 1)
	for _, cfg := range []LeasePoolConfig{{}, {Key: "x", Kind: "role", Capacity: -1}, {Key: "x", Kind: "role", Capacity: 1, TTL: -1}} {
		if e := s.EnsureLeasePool(context.Background(), audit(), cfg); e == nil {
			t.Fatal("bad config accepted")
		}
	}
	if e := s.ReleaseLease(context.Background(), audit(), "x", "y", scheduler.Owner{}, ""); e == nil {
		t.Fatal("no reason accepted")
	}
}
func TestCorruptSQLLeaseLedgerFailsClosed(t *testing.T) {
	s, m := fixture(t, 1)
	m.ExpectBegin()
	m.ExpectQuery("SELECT version,payload").WillReturnRows(sqlmock.NewRows([]string{"version", "payload", "kind"}).AddRow(1, `{"Capacity":-1}`, "role"))
	m.ExpectRollback()
	if _, e := s.AdvanceLeases(context.Background(), audit(), "x", time.Now(), map[scheduler.Owner]scheduler.Liveness{}); e == nil {
		t.Fatal("corrupt pool admitted")
	}
}
func TestLeaseSQLCASFailureRollsBackAudit(t *testing.T) {
	s, m := fixture(t, 1)
	p, _ := scheduler.NewPool(2)
	b, _ := json.Marshal(p)
	m.ExpectBegin()
	m.ExpectQuery("SELECT version,payload").WillReturnRows(sqlmock.NewRows([]string{"version", "payload", "kind"}).AddRow(1, string(b), "role"))
	m.ExpectExec("UPDATE resource_pools").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectRollback()
	if e := s.EnqueueLease(context.Background(), audit(), "x", scheduler.Request{ID: "r", Owner: scheduler.Owner{PID: 1, Started: 1}, Weight: 1, Queued: time.Now()}); e != ErrConflict {
		t.Fatal(e)
	}
}
func exerciseDurableLeases(t *testing.T, ctx context.Context, s *Store, dsn string) {
	t.Helper()
	a := Audit{Actor: "lease-test", Action: "lease-mutate", Outcome: "ok"}
	now := time.Now().UTC()
	o := scheduler.Owner{PID: 100, Started: 1}
	for i := 0; i < 8; i++ {
		if e := s.EnqueueLease(ctx, a, "durable-role", scheduler.Request{ID: fmt.Sprint(i), Owner: o, Weight: 1, Queued: now.Add(time.Duration(i) * time.Millisecond)}); e != nil {
			t.Fatal(e)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, e := Open(ctx, dsn, Options{Attempts: 20})
			if e != nil {
				errs <- e
				return
			}
			defer other.Close()
			_, e = other.AdvanceLeases(ctx, a, "durable-role", now, map[scheduler.Owner]scheduler.Liveness{})
			if e != nil {
				errs <- e
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	p, e := s.LeaseSnapshot(ctx, "durable-role")
	if e != nil || len(p.Holders) != 2 || len(p.Waiting) != 6 || p.Holders[0].ID != "0" || p.Holders[1].ID != "1" {
		t.Fatal(p, e)
	}
	if e = s.ReleaseLease(ctx, a, "durable-role", "0", scheduler.Owner{PID: 100, Started: 2}, "PID reused"); e == nil {
		t.Fatal("wrong owner released")
	}
	if e = s.ReleaseLease(ctx, a, "durable-role", "0", o, "process exited"); e != nil {
		t.Fatal(e)
	}
	u, e := s.AdvanceLeases(ctx, a, "durable-role", now.Add(time.Second), map[scheduler.Owner]scheduler.Liveness{})
	if e != nil || len(u.Granted) != 1 || u.Granted[0].ID != "2" {
		t.Fatal(u, e)
	}
	u, e = s.AdvanceLeases(ctx, a, "durable-role", now.Add(time.Second), map[scheduler.Owner]scheduler.Liveness{o: scheduler.Dead})
	if e != nil || len(u.Pool.Holders) != 0 || len(u.Pool.Waiting) != 0 {
		t.Fatal(u, e)
	}
	if e = s.EnqueueLease(ctx, a, "durable-role", scheduler.Request{ID: "cancel", Owner: o, Weight: 1, Queued: now}); e != nil {
		t.Fatal(e)
	}
	if e = s.CancelLease(ctx, a, "durable-role", "cancel", o, "request context cancelled"); e != nil {
		t.Fatal(e)
	}
	p, e = s.LeaseSnapshot(ctx, "durable-role")
	if e != nil || len(p.Waiting) != 0 {
		t.Fatal("cancel leaked waiter", p, e)
	}
	var count int
	if e = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor='lease-test'`).Scan(&count); e != nil || count != 21 {
		t.Fatal("lease audit", count, e)
	}
}

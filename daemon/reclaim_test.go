package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/scheduler"
)

// deadOwner returns the identity of a process that has already exited.
func deadOwner(t *testing.T) scheduler.Owner {
	t.Helper()
	cmd := exec.Command("true")
	if e := cmd.Run(); e != nil {
		t.Fatal(e)
	}
	o := scheduler.Owner{PID: cmd.Process.Pid, Started: time.Now().Add(-time.Minute).Unix()}
	if scheduler.ProbeOwner(o) != scheduler.Dead {
		t.Skip("exited pid was reused")
	}
	return o
}

type reclaimResult struct {
	DryRun   bool            `json:"dry_run"`
	HostWide bool            `json:"host_wide"`
	Ledgers  []ReclaimLedger `json:"ledgers"`
}

func (r reclaimResult) ledger(name string) ReclaimLedger {
	for _, l := range r.Ledgers {
		if l.Ledger == name {
			return l
		}
	}
	return ReclaimLedger{}
}

func TestReclaimRoleSlotsDryRunHostWideAuthorityAndAudit(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	setup := controlplane.Audit{Actor: "test", Action: "plant role pool", Outcome: "ok"}
	key := "fixture-workflow/worker"
	if e = d.store.EnsureLeasePool(ctx, setup, controlplane.LeasePoolConfig{Key: key, Kind: "role", Capacity: 2}); e != nil {
		t.Fatal(e)
	}
	alive, e := scheduler.CurrentOwner()
	if e != nil {
		t.Fatal(e)
	}
	dead := deadOwner(t)
	for id, o := range map[string]scheduler.Owner{"live-slot": alive, "dead-slot": dead} {
		if e = d.store.EnqueueLease(ctx, setup, key, scheduler.Request{ID: id, Owner: o, Weight: 1, Queued: time.Now().UTC()}); e != nil {
			t.Fatal(e)
		}
	}
	if u, e := d.store.AdvanceLeases(ctx, setup, key, time.Now().UTC(), map[scheduler.Owner]scheduler.Liveness{alive: scheduler.Alive, dead: scheduler.Alive}); e != nil || len(u.Granted) != 2 {
		t.Fatal(u, e)
	}
	if _, e = call(t, d, "Reclaim", ReclaimParams{DryRun: true}, ""); e == nil {
		t.Fatal("reclaim without authorizing message accepted")
	}
	var dry reclaimResult
	r, e := call(t, d, "Reclaim", ReclaimParams{DryRun: true, AuthorizingMessage: "message"}, "")
	decodeResult(t, r, e, &dry)
	roles := dry.ledger("role-slots")
	if !dry.DryRun || roles.Status != "dry-run" || len(roles.Reclaimed) != 1 || roles.Reclaimed[0].ID != "dead-slot" || roles.Reclaimed[0].Pool != key || len(roles.Kept) != 1 || roles.Kept[0].Liveness != "alive" {
		t.Fatalf("dry run %s", r.Result)
	}
	for _, name := range []string{"heavy-op", "panes"} {
		if l := dry.ledger(name); l.Status != "skipped" || l.Scope != "host" {
			t.Fatalf("%s without --host-wide: %+v", name, l)
		}
	}
	if p, e := d.store.LeaseSnapshot(ctx, key); e != nil || len(p.Holders) != 2 {
		t.Fatal("dry run changed the pool", p.Holders, e)
	}
	if _, e = call(t, d, "Reclaim", ReclaimParams{HostWide: true, AuthorizingMessage: "message"}, ""); e == nil || !strings.Contains(e.Error(), "host-wide") {
		t.Fatalf("host-wide without explicit class: %v", e)
	}
	var applied reclaimResult
	r, e = call(t, d, "Reclaim", ReclaimParams{AuthorizingMessage: "message"}, "")
	decodeResult(t, r, e, &applied)
	if roles = applied.ledger("role-slots"); roles.Status != "reconciled" || len(roles.Reclaimed) != 1 {
		t.Fatalf("apply %s", r.Result)
	}
	if p, e := d.store.LeaseSnapshot(ctx, key); e != nil || len(p.Holders) != 1 || p.Holders[0].ID != "live-slot" {
		t.Fatal("dead slot kept or live slot reclaimed", p.Holders, e)
	}
	if _, e = call(t, d, "Message", MessageParams{ID: "host", Text: `{"text":"Reclaim host-wide ledgers","authorize":{"classes":["host-wide"]}}`}, ""); e != nil {
		t.Fatal(e)
	}
	var host reclaimResult
	r, e = call(t, d, "Reclaim", ReclaimParams{HostWide: true, AuthorizingMessage: "host"}, "")
	decodeResult(t, r, e, &host)
	if !host.HostWide || host.ledger("heavy-op").Status != "not-configured" || len(host.ledger("role-slots").Reclaimed) != 0 || len(host.ledger("role-slots").Kept) != 1 {
		t.Fatalf("host-wide %s", r.Result)
	}
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect reclaim audit", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		for _, c := range []struct {
			message, outcome string
			want             int
		}{{"message", "reclaimed role-slots " + key + "/dead-slot", 1}, {"message", "ok", 1}, {"host", "ok", 1}} {
			var n int
			if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor='operator' AND action='Reclaim' AND message_id=? AND outcome=?`, c.message, c.outcome).Scan(&n); e != nil {
				return e
			}
			if n != c.want {
				return fmt.Errorf("%+v audit rows %d", c, n)
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

func TestReclaimHostWideHeavyOpLedgerLive(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	if _, e = call(t, d, "Message", MessageParams{ID: "host", Text: `{"text":"Reclaim host-wide ledgers","authorize":{"classes":["host-wide"]}}`}, ""); e != nil {
		t.Fatal(e)
	}
	ledger, e := scheduler.OpenDynamicLedger(cfg.Live.HostLedger, cfg.Live.HeavyCapacity)
	if e != nil {
		t.Fatal(e)
	}
	dead := deadOwner(t)
	if e = ledger.Enqueue(ctx, scheduler.Request{ID: "crashed-build", Owner: dead, Weight: 1, Queued: time.Now().UTC()}); e != nil {
		t.Fatal(e)
	}
	if granted, _, e := ledger.Advance(ctx, time.Now().UTC(), map[scheduler.Owner]scheduler.Liveness{dead: scheduler.Alive}); e != nil || len(granted) != 1 {
		t.Fatal(granted, e)
	}
	var dry, applied reclaimResult
	r, e := call(t, d, "Reclaim", ReclaimParams{HostWide: true, DryRun: true, AuthorizingMessage: "host"}, "")
	decodeResult(t, r, e, &dry)
	if heavy := dry.ledger("heavy-op"); heavy.Status != "dry-run" || len(heavy.Reclaimed) != 1 || heavy.Reclaimed[0].ID != "crashed-build" {
		t.Fatalf("dry run %s", r.Result)
	}
	if sessions := dry.ledger("sessions"); sessions.Status != "dry-run" {
		t.Fatalf("sessions ledger %s", r.Result)
	}
	r, e = call(t, d, "Reclaim", ReclaimParams{HostWide: true, AuthorizingMessage: "host"}, "")
	decodeResult(t, r, e, &applied)
	if heavy := applied.ledger("heavy-op"); heavy.Status != "reconciled" || len(heavy.Reclaimed) != 1 {
		t.Fatalf("apply %s", r.Result)
	}
	if p, e := ledger.Snapshot(ctx); e != nil || len(p.Holders) != 0 {
		t.Fatal("dead heavy-op holder kept", p.Holders, e)
	}
}

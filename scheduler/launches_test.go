package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/processowner"
)

// FAE-HOST-006 without a pane ledger: every launch still in its warm-up,
// host-wide across instances sharing the ledger, counts as pending, so a
// fan-out cannot each pass the floor alone.
func TestLaunchReservationCountsPendingHostWide(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(physicalTemp(t), "host")
	one, e := OpenLedger(dir, 2)
	if e != nil {
		t.Fatal(e)
	}
	two, e := OpenLedger(dir, 2)
	if e != nil {
		t.Fatal(e)
	}
	o, e := CurrentOwner()
	if e != nil {
		t.Fatal(e)
	}
	birth, e := processowner.Birth(o.PID)
	if e != nil {
		t.Fatal(e)
	}
	cfg := hostguard.DefaultConfig()
	cfg.Soft, cfg.PerPane = 2*hostguard.GiB, hostguard.GiB
	// Hard floor 2 GiB with swap; room for 2.5 sessions.
	observe := func(context.Context) (hostguard.Sample, error) {
		return hostguard.Sample{Memory: hostguard.Reading{Value: float64(9 * hostguard.GiB / 2), Known: true}, Swap: hostguard.Reading{Value: float64(hostguard.GiB), Known: true}}, nil
	}
	now := time.Now().UTC()
	reserve := func(l *Ledger, id string, at time.Time) error {
		return l.ReserveLaunch(ctx, LaunchReservation{ID: id, Owner: o, Birth: birth, Reserved: at}, observe, cfg, at)
	}
	if e = reserve(one, "a", now); e != nil {
		t.Fatal(e)
	}
	if e = reserve(two, "b", now); e != nil {
		t.Fatal(e)
	}
	var refusal *AdmissionRefusal
	if e = reserve(one, "c", now); !errors.As(e, &refusal) || refusal.Reason != "MemoryReservation" {
		t.Fatal("third concurrent launch passed the floor alone", e)
	}
	if e = reserve(one, "a", now); e == nil {
		t.Fatal("duplicate reservation")
	}
	// A launch that never started is withdrawn at once; the others drain at
	// the end of their warm-up.
	if e = two.ReleaseLaunch(ctx, "b", o); e != nil {
		t.Fatal(e)
	}
	if e = reserve(one, "c", now); e != nil {
		t.Fatal(e)
	}
	if e = reserve(one, "d", now.Add(LaunchWarmup)); e != nil {
		t.Fatal("warm-up never ended", e)
	}
	if e = one.ReleaseLaunch(ctx, "d", Owner{PID: o.PID, Started: o.Started + 1}); e == nil {
		t.Fatal("foreign owner released a launch reservation")
	}
	// A dead owner's reservation is reconciled on the next access.
	dead := LaunchReservation{ID: "dead", Owner: Owner{PID: o.PID, Started: o.Started}, Birth: "not-" + birth, Reserved: now}
	if e = one.ReserveLaunch(ctx, dead, observe, cfg, now); e != nil {
		t.Fatal(e)
	}
	n, e := one.PendingLaunches(ctx, now)
	if e != nil || n != 1 {
		t.Fatal("dead owner retained", n, e)
	}
}

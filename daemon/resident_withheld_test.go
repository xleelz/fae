package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/xleelz/fae/engine"
)

// FAE-SCH-010, FAE-RT-012: a resident turn deferred because its slot's
// retirement stayed unproven withholds its task as resident_unretired, not as
// host_guard. The runtime reports it through ReconciliationDeferred with the
// turn's launch context, which names the task.
func TestResidentReconciliationDeferralWithheldByCause(t *testing.T) {
	a := admissionFixture(t, 1)
	a.view.Store(&scheduleView{molecule: map[string]string{"resident-task": "m", "other-task": "m"}})
	parked := func(task string) engine.Job {
		return engine.Job{TaskID: task, Parked: true, Reason: "infrastructure-deferred"}
	}
	if r := parkedReason(parked("resident-task"), a); r != withheldHostGuard {
		t.Fatalf("fixture: an unclassified deferral withheld as %q", r)
	}
	cause := errors.New("resident identity unavailable; retirement required")
	a.residentReconciliationDeferred(context.Background(), "resident-1", cause)
	a.residentReconciliationDeferred(withWithheldTask(context.Background(), "resident-task"), "resident-1", cause)
	if r := parkedReason(parked("resident-task"), a); r != withheldResidentUnretired {
		t.Fatalf("resident reconciliation deferral withheld as %q", r)
	}
	if r := parkedReason(parked("other-task"), a); r != withheldHostGuard {
		t.Fatalf("another task's deferral withheld as %q", r)
	}
}

// The live resident adapter reports reconciliation deferrals to admission.
func TestResidentSessionsReportReconciliationDeferrals(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.Live.Adapter = "multiplexer"
	cfg.Live.HerdrBinary = testHerdr(t)
	cfg.Live.ResidentSessions = true
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	d.mu.Lock()
	wired := d.live != nil && d.live.resident != nil && d.live.resident.ReconciliationDeferred != nil
	d.mu.Unlock()
	if !wired {
		t.Fatal("resident reconciliation deferrals are not reported to admission")
	}
}

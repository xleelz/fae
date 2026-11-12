package daemon

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
)

func occupyHost(t *testing.T, a *liveAdmission, owner scheduler.Owner) {
	t.Helper()
	ctx := context.Background()
	if e := a.host.Enqueue(ctx, scheduler.Request{ID: "foreign-build", Owner: owner, Weight: 1, Queued: time.Now()}); e != nil {
		t.Fatal(e)
	}
	if _, _, e := a.host.Advance(ctx, time.Now(), map[scheduler.Owner]scheduler.Liveness{}); e != nil {
		t.Fatal(e)
	}
}

// FAE-SCH-004, §1.2, §14: agent sessions are not heavy ops. A full heavy pool
// never caps them, while a leased effect still waits for capacity.
func TestLiveAdmissionAgentSessionsTakeNoHeavyLease(t *testing.T) {
	a := admissionFixture(t, 1)
	o, e := scheduler.CurrentOwner()
	if e != nil {
		t.Fatal(e)
	}
	occupyHost(t, a, o)
	ctx := context.Background()
	for _, p := range []workflow.Primitive{workflow.AgentTurn, workflow.Review} {
		release, e := a.Acquire(ctx, engine.Dispatch{TaskID: "agent", Config: workflow.Stage{Primitive: p, Weight: 2}}, execution.Binding{}, engine.Session{ID: "agent-" + string(p)})
		if e != nil {
			t.Fatal(p, e)
		}
		assertHostCounts(t, a, 1, 0)
		if e = release(); e != nil {
			t.Fatal(e)
		}
	}
	wait, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if _, e = a.Acquire(wait, engine.Dispatch{TaskID: "effect", Config: workflow.Stage{Primitive: workflow.ExternalEffect, Weight: 1}}, execution.Binding{}, engine.Session{ID: "effect"}); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("leased effect bypassed a full pool", e)
	}
	assertHostCounts(t, a, 1, 0)
}

// FAE-HOST-010: daemon-internal I/O operations pass the two-reading I/O gate
// before the heavy-op lease, in the controller class.
func TestLiveAdmissionHostIOMaintenancePassesGate(t *testing.T) {
	a, clock, _ := pressureFixture(t, 1)
	var seen []poolView
	a.observe = func(context.Context) (hostguard.Sample, error) {
		seen = append(seen, poolCounts(t, a))
		return hostSample(1, 0.5), nil
	}
	release, e := a.AcquireHostIO(context.Background(), "plane-backup-1")
	if e != nil {
		t.Fatal(e)
	}
	if len(seen) != 12 || clock.elapsed(pressureEpoch) != 30*time.Second {
		t.Fatal(seen, clock.elapsed(pressureEpoch))
	}
	for _, v := range seen[:11] {
		if v != (poolView{0, 0}) {
			t.Fatal("gate reading saw a lease", seen)
		}
	}
	p, e := a.host.Snapshot(context.Background())
	if e != nil || len(p.Holders) != 1 || p.Holders[0].Class != controllerClass {
		t.Fatal(p, e)
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
	assertHostCounts(t, a, 0, 0)
}

// FAE-HOST-031: a foreign heavy-pool owner proven dead by pid and start time is
// reclaimed on the next access; an ambiguous owner keeps its capacity. This
// daemon never probes itself.
func TestLiveAdmissionDeadForeignHeavyOwnerReclaimed(t *testing.T) {
	a := admissionFixture(t, 1)
	foreign := scheduler.Owner{PID: 4242, Started: 77}
	occupyHost(t, a, foreign)
	var probed []scheduler.Owner
	a.probe = func(o scheduler.Owner) scheduler.Liveness { probed = append(probed, o); return scheduler.Unknown }
	ctx := context.Background()
	wait, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	dispatch := engine.Dispatch{TaskID: "heavy", Config: workflow.Stage{Weight: 1}}
	if _, e := a.Acquire(wait, dispatch, execution.Binding{}, engine.Session{ID: "ambiguous"}); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("ambiguous owner reclaimed", e)
	}
	assertHostCounts(t, a, 1, 0)
	if len(probed) == 0 {
		t.Fatal("foreign owner never probed")
	}
	for _, o := range probed {
		if o != foreign {
			t.Fatal("probed", o)
		}
	}
	b := &liveAdmission{daemon: a.daemon, host: a.host, owner: a.owner, observe: a.observe, probe: func(o scheduler.Owner) scheduler.Liveness {
		if o == foreign {
			return scheduler.Dead
		}
		return scheduler.Unknown
	}}
	release, e := b.Acquire(ctx, dispatch, execution.Binding{}, engine.Session{ID: "after-death"})
	if e != nil {
		t.Fatal("dead owner retained capacity", e)
	}
	p, e := a.host.Snapshot(ctx)
	if e != nil || len(p.Holders) != 1 || p.Holders[0].Owner != a.owner {
		t.Fatal(p, e)
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
	audit, e := a.host.Audit(ctx)
	if e != nil {
		t.Fatal(e)
	}
	reclaimed := false
	for _, entry := range audit {
		for _, ev := range entry.Events {
			reclaimed = reclaimed || (ev.Kind == "Reclaimed" && ev.RequestID == "foreign-build")
		}
	}
	if !reclaimed {
		t.Fatal("reclamation not audited", audit)
	}
}

func paneFixture(t *testing.T, a *liveAdmission, limit int) {
	t.Helper()
	var e error
	if a.owner, e = scheduler.CurrentOwner(); e != nil {
		t.Fatal(e)
	}
	if a.birth, e = processowner.Birth(a.owner.PID); e != nil {
		t.Fatal(e)
	}
	a.paneLimit = limit
	if a.panes, e = scheduler.OpenPaneLedger(filepath.Join(a.daemon.layout.Root, "panes"), limit); e != nil {
		t.Fatal(e)
	}
}

// FAE-HOST-002, -006: panes at the limit and the per-pane memory reservation
// back off (bounded) before refusing; capacity freed during the back-off is
// taken without a park.
func TestLiveAdmissionPaneAndMemoryRefusalsBackOff(t *testing.T) {
	agent := engine.Dispatch{TaskID: "agent", Config: workflow.Stage{Primitive: workflow.AgentTurn}}
	for _, freeAfter := range []int{4, 0} {
		a, clock, _ := pressureFixture(t, 1)
		paneFixture(t, a, 3)
		a.observe = func(context.Context) (hostguard.Sample, error) { return hostSample(1, 0.5), nil }
		inventories := 0
		a.inventory = func(context.Context) (map[string]int, error) {
			inventories++
			if freeAfter > 0 && inventories > freeAfter {
				return map[string]int{}, nil
			}
			return map[string]int{"/foreign": 3}, nil
		}
		release, e := a.Acquire(context.Background(), agent, execution.Binding{}, engine.Session{ID: "pane-backoff"})
		if freeAfter == 0 {
			if refusalReason(t, e) != "PaneLimit" || inventories != 11 || len(clock.slept) != 10 {
				t.Fatal("sustained pane limit", e, inventories, clock.slept)
			}
			continue
		}
		if e != nil || inventories != freeAfter+1 || len(clock.slept) != freeAfter {
			t.Fatal("pane freed during back-off", e, inventories, clock.slept)
		}
		if e = release(); e != nil {
			t.Fatal(e)
		}
	}
	// Direct sessions read once before the reservation and once under the
	// host ledger lock, so a step takes two readings.
	for _, clearStep := range []int{3, 0} {
		a, clock, _ := pressureFixture(t, 1)
		cfg := hostguard.DefaultConfig()
		cfg.Soft, cfg.Hard, cfg.HardNoSwap, cfg.PerPane = 2*hostguard.GiB, 2*hostguard.GiB, 2*hostguard.GiB, hostguard.GiB
		if e := a.host.ConfigureHost(context.Background(), a.daemon.layout.Root, 1, cfg); e != nil {
			t.Fatal(e)
		}
		readings := 0
		a.observe = func(context.Context) (hostguard.Sample, error) {
			readings++
			s := hostSample(1, 0.5)
			s.Memory.Value = float64(5 * hostguard.GiB / 2)
			if clearStep > 0 && readings > 2*(clearStep-1) {
				s.Memory.Value = float64(4 * hostguard.GiB)
			}
			return s, nil
		}
		release, e := a.Acquire(context.Background(), agent, execution.Binding{}, engine.Session{ID: "memory-backoff"})
		if clearStep == 0 {
			if refusalReason(t, e) != "MemoryReservation" || readings != 22 || len(clock.slept) != 10 {
				t.Fatal("sustained reservation", e, readings, clock.slept)
			}
			continue
		}
		if e != nil || len(clock.slept) != clearStep-1 {
			t.Fatal("memory freed during back-off", e, readings, clock.slept)
		}
		if e = release(); e != nil {
			t.Fatal(e)
		}
	}
}

// FAE-HOST-004: unreadable memory available proceeds and records the gap on
// both admission paths.
func TestLiveAdmissionUnreadableMemoryProceedsWithGap(t *testing.T) {
	for _, p := range []workflow.Primitive{workflow.HostCommand, workflow.AgentTurn} {
		a, _, logPath := pressureFixture(t, 1)
		a.observe = func(context.Context) (hostguard.Sample, error) {
			s := hostSample(1, 0.5)
			s.Memory = hostguard.Reading{}
			return s, nil
		}
		release, e := a.Acquire(context.Background(), heavyDispatch(p), execution.Binding{}, engine.Session{ID: "memory-gap-" + string(p)})
		if e != nil {
			t.Fatal(p, e)
		}
		if e = release(); e != nil {
			t.Fatal(e)
		}
		if countContaining(hostAdmissionLines(t, logPath), "gate=pressure", "outcome=proceed", "gap=true", "memory_available=unknown", "missing=memory_available") == 0 {
			t.Fatal(p, "memory gap not recorded")
		}
	}
}

// FAE-HOST-003: a hard refusal parks without back-off and raises one alert
// per hard-pressure episode; a reading clear of the breaker ends the episode.
func TestLiveAdmissionHardLimitRaisesAlert(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	a := admissionFixture(t, 1)
	a.daemon = d
	hard := true
	a.observe = func(context.Context) (hostguard.Sample, error) {
		s := hostSample(1, 0.5)
		if hard {
			s.Memory.Value = float64(hostguard.GiB)
		}
		return s, nil
	}
	alerts := func() int {
		var n int
		if e := d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "count host alerts", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM alerts WHERE source='host-admission'").Scan(&n)
		}); e != nil {
			t.Fatal(e)
		}
		return n
	}
	for i, want := range []int{1, 1} {
		if _, e = a.Acquire(ctx, heavyDispatch(workflow.HostCommand), execution.Binding{}, engine.Session{ID: "hard-" + string(rune('a'+i))}); refusalReason(t, e) != "HostHardLimit" || alerts() != want {
			t.Fatal(i, e, alerts())
		}
	}
	hard = false
	release, e := a.Acquire(ctx, heavyDispatch(workflow.AgentTurn), execution.Binding{}, engine.Session{ID: "clear"})
	if e != nil {
		t.Fatal(e)
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
	hard = true
	if _, e = a.Acquire(ctx, heavyDispatch(workflow.AgentTurn), execution.Binding{}, engine.Session{ID: "hard-again"}); refusalReason(t, e) != "HostHardLimit" || alerts() != 2 {
		t.Fatal("new episode not alerted", e, alerts())
	}
	events, e := d.store.Events(ctx, 0, 1000)
	if e != nil {
		t.Fatal(e)
	}
	found := 0
	for _, ev := range events {
		if ev.Kind == "alert-raised" && strings.Contains(string(ev.Payload), "HostHardLimit") {
			found++
		}
	}
	if found != 2 {
		t.Fatal("alert events", found)
	}
}

// A new resident pane passes the pressure hold and back-off like a one-shot
// pane: no reservation is attempted while the hold is in force.
func TestReserveResidentPaneHonoursPressureHold(t *testing.T) {
	a, clock, _ := pressureFixture(t, 1)
	paneFixture(t, a, 3)
	inventories := 0
	a.inventory = func(context.Context) (map[string]int, error) { inventories++; return map[string]int{}, nil }
	c := hostguard.DefaultConfig()
	pressured := true
	a.observe = func(context.Context) (hostguard.Sample, error) {
		if pressured {
			return hostSample(1, c.Load+1), nil
		}
		return hostSample(1, 0.5), nil
	}
	socket := filepath.Join(a.daemon.layout.Root, "resident.sock")
	if e := a.ReserveResidentPane(context.Background(), "resident-1", socket); refusalReason(t, e) != "HostPressure" || inventories != 0 || len(clock.slept) != 10 {
		t.Fatal("resident reserved under pressure", e, inventories, clock.slept)
	}
	pressured = false
	if e := a.ReserveResidentPane(context.Background(), "resident-1", socket); e != nil || inventories != 1 {
		t.Fatal("hold not lifted by two clear readings", e, inventories)
	}
	if rows, e := a.panes.Snapshot(context.Background()); e != nil || len(rows) != 1 || rows[0].Socket != socket {
		t.Fatal(rows, e)
	}
}

// FAE-HOST-006 for Direct sessions: launches still in their warm-up count as
// pending, so a fan-out that each pass the floor alone is deferred, and a
// released launch frees its reservation.
func TestLiveAdmissionDirectLaunchesCountPending(t *testing.T) {
	a, clock, _ := pressureFixture(t, 1)
	var e error
	if a.owner, e = scheduler.CurrentOwner(); e != nil {
		t.Fatal(e)
	}
	if a.birth, e = processowner.Birth(a.owner.PID); e != nil {
		t.Fatal(e)
	}
	cfg := hostguard.DefaultConfig()
	cfg.Soft, cfg.Hard, cfg.HardNoSwap, cfg.PerPane = 2*hostguard.GiB, 2*hostguard.GiB, 2*hostguard.GiB, hostguard.GiB
	if e = a.host.ConfigureHost(context.Background(), a.daemon.layout.Root, 1, cfg); e != nil {
		t.Fatal(e)
	}
	a.observe = func(context.Context) (hostguard.Sample, error) {
		s := hostSample(1, 0.5)
		s.Memory.Value = float64(9 * hostguard.GiB / 2)
		return s, nil
	}
	ctx := context.Background()
	var releases []func() error
	for _, id := range []string{"direct-1", "direct-2"} {
		release, e := a.Acquire(ctx, agentTurn(id), execution.Binding{}, engine.Session{ID: id})
		if e != nil {
			t.Fatal(id, e)
		}
		releases = append(releases, release)
	}
	if n, e := a.host.PendingLaunches(ctx, time.Now()); e != nil || n != 2 {
		t.Fatal(n, e)
	}
	if _, e = a.Acquire(ctx, agentTurn("direct-3"), execution.Binding{}, engine.Session{ID: "direct-3"}); refusalReason(t, e) != "MemoryReservation" || len(clock.slept) != 10 {
		t.Fatal("fan-out passed the floor", e, clock.slept)
	}
	if n, e := a.host.PendingLaunches(ctx, time.Now()); e != nil || n != 2 {
		t.Fatal("refused launch kept a reservation", n, e)
	}
	if e = releases[0](); e != nil {
		t.Fatal(e)
	}
	release, e := a.Acquire(ctx, agentTurn("direct-4"), execution.Binding{}, engine.Session{ID: "direct-4"})
	if e != nil {
		t.Fatal("released launch still reserved", e)
	}
	for _, r := range []func() error{releases[1], release} {
		if e = r(); e != nil {
			t.Fatal(e)
		}
	}
	if n, e := a.host.PendingLaunches(ctx, time.Now()); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}

package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
)

func admissionFixture(t *testing.T, capacity int) *liveAdmission {
	t.Helper()
	root := reviewPhysical(t)
	l, e := scheduler.OpenLedger(filepath.Join(root, "host"), capacity)
	if e != nil {
		t.Fatal(e)
	}
	return &liveAdmission{daemon: &Daemon{layout: Layout{Root: root}}, host: l, owner: scheduler.Owner{PID: 123, Started: 456}, birth: "admission-test", observe: func(context.Context) (hostguard.Sample, error) {
		// Explicit injected telemetry is local to admission tests. Real launches use
		// hostguard.Observe; these tests never create an executable process.
		return hostguard.Sample{Memory: hostguard.Reading{Value: float64(32 * hostguard.GiB), Known: true}}, nil
	}}
}
func assertHostCounts(t *testing.T, a *liveAdmission, holders, waiters int) {
	t.Helper()
	p, e := a.host.Snapshot(context.Background())
	if e != nil || len(p.Holders) != holders || len(p.Waiting) != waiters {
		t.Fatalf("host holders=%d waiting=%d: %+v %v", holders, waiters, p, e)
	}
}

// A leased operation backs off and samples before it queues (FAE-SCH-008); a
// saturated pool is waited for without any launch reading, and cancellation
// leaves the live foreign holder untouched.
func TestLiveAdmissionHostSaturationCancellationNoLaunch(t *testing.T) {
	a := admissionFixture(t, 1)
	o, e := scheduler.CurrentOwner()
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	if e := a.host.Enqueue(ctx, scheduler.Request{ID: "existing", Owner: o, Weight: 1, Queued: time.Now()}); e != nil {
		t.Fatal(e)
	}
	if _, _, e := a.host.Advance(ctx, time.Now(), map[scheduler.Owner]scheduler.Liveness{}); e != nil {
		t.Fatal(e)
	}
	var readings atomic.Int32
	a.observe = func(context.Context) (hostguard.Sample, error) {
		readings.Add(1)
		assertHostCounts(t, a, 1, 0)
		return hostSample(1, 0.5), nil
	}
	wait, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	release, e := a.Acquire(wait, engine.Dispatch{TaskID: "waiting"}, execution.Binding{}, engine.Session{ID: "waiting"})
	if e == nil || release != nil || !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("saturated admission returned permit: %v %v", release != nil, e)
	}
	if readings.Load() != 1 {
		t.Fatal("launch reading taken without the lease", readings.Load())
	}
	assertHostCounts(t, a, 1, 0)
}

// Failures before the lease request take no lease; failures of the reading
// under the granted lease release it.
func TestLiveAdmissionRejectionReleasesHostReservation(t *testing.T) {
	for _, kind := range []string{"severe-memory", "observer-error", "cancelled-after-grant"} {
		for _, granted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-granted-%t", kind, granted), func(t *testing.T) {
				a := admissionFixture(t, 1)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				a.observe = func(context.Context) (hostguard.Sample, error) {
					p, e := a.host.Snapshot(context.Background())
					if e != nil {
						t.Fatal(e)
					}
					if len(p.Holders) == 0 && granted {
						return hostSample(1, 0.5), nil
					}
					switch kind {
					case "observer-error":
						return hostguard.Sample{}, errors.New("telemetry execution failed")
					case "cancelled-after-grant":
						cancel()
						return hostSample(1, 0.5), nil
					}
					return hostguard.Sample{Memory: hostguard.Reading{Value: 1, Known: true}}, nil
				}
				release, e := a.Acquire(ctx, engine.Dispatch{TaskID: "reject"}, execution.Binding{}, engine.Session{ID: "reject"})
				if e == nil || release != nil {
					t.Fatalf("rejection returned launch permission: %v %v", release != nil, e)
				}
				assertHostCounts(t, a, 0, 0)
			})
		}
	}
}
func TestLiveAdmissionWeightAndUnscopedRelease(t *testing.T) {
	a := admissionFixture(t, 2)
	if _, e := a.Acquire(context.Background(), engine.Dispatch{Config: workflow.Stage{Weight: 3}}, execution.Binding{}, engine.Session{ID: "overweight"}); e == nil {
		t.Fatal("overweight admission accepted")
	}
	assertHostCounts(t, a, 0, 0)
	release, e := a.Acquire(context.Background(), engine.Dispatch{Config: workflow.Stage{Weight: 2}}, execution.Binding{}, engine.Session{ID: "weight-two"})
	if e != nil {
		t.Fatal(e)
	}
	p, e := a.host.Snapshot(context.Background())
	if e != nil || len(p.Holders) != 1 || p.Holders[0].Weight != 2 {
		t.Fatal(p, e)
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
	assertHostCounts(t, a, 0, 0)
}
func roleTestKey(doc workflow.Document, task, role string) string {
	scope := task
	if doc.Roles[role].Scope == "workflow-global" {
		scope = "global"
	}
	return fmt.Sprintf("role/%x", sha256.Sum256([]byte(doc.ID+"\x00"+scope+"\x00"+role)))
}

// FAE-WF-040: a role declared with cap -1 is unbounded live: every session
// launches without a role slot, in either scope, while other admission
// (host leases) still applies.
func TestLiveAdmissionUnboundedRoleCap(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	for _, scope := range []string{"workflow-global", "task-bound"} {
		a := admissionFixture(t, 4)
		a.daemon = d
		doc := workflow.Document{ID: "unbounded-" + scope, Roles: map[string]workflow.Role{"coder": {Scope: scope, Cap: -1}}}
		b := execution.Binding{Document: doc}
		var releases []func() error
		for i := 0; i < 3; i++ {
			wait, cancel := context.WithTimeout(ctx, 10*time.Second)
			release, e := a.Acquire(wait, engine.Dispatch{TaskID: "first"}, b, engine.Session{ID: fmt.Sprintf("%s-%d", scope, i), Role: "coder"})
			cancel()
			if e != nil {
				t.Fatalf("%s: unbounded role refused session %d: %v", scope, i, e)
			}
			releases = append(releases, release)
		}
		if p, e := d.store.LeaseSnapshot(ctx, roleTestKey(doc, "first", "coder")); e == nil && len(p.Holders) != 0 {
			t.Fatal("unbounded role took a slot", p)
		}
		assertHostCounts(t, a, 3, 0)
		for _, release := range releases {
			if e = release(); e != nil {
				t.Fatal(e)
			}
		}
		assertHostCounts(t, a, 0, 0)
	}
}
func TestLiveAdmissionDurableRoleScopesAndCancellation(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	for _, scope := range []string{"workflow-global", "task-bound"} {
		t.Run(scope, func(t *testing.T) {
			a := admissionFixture(t, 4)
			a.daemon = d
			doc := workflow.Document{ID: "lease-test-" + scope, Roles: map[string]workflow.Role{"coder": {Scope: scope, Cap: 1}}}
			b := execution.Binding{Document: doc}
			first := engine.Dispatch{TaskID: "first"}
			second := engine.Dispatch{TaskID: "second"}
			release1, e := a.Acquire(ctx, first, b, engine.Session{ID: scope + "-one", Role: "coder"})
			if e != nil {
				t.Fatal(e)
			}
			released := false
			defer func() {
				if !released {
					_ = release1()
				}
			}()
			key := roleTestKey(doc, "first", "coder")
			p, e := d.store.LeaseSnapshot(ctx, key)
			if e != nil || len(p.Holders) != 1 {
				t.Fatal("permit returned before role lease", p, e)
			}
			wait, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
			defer cancel()
			release2, e := a.Acquire(wait, second, b, engine.Session{ID: scope + "-two", Role: "coder"})
			if scope == "workflow-global" {
				if !errors.Is(e, context.DeadlineExceeded) || release2 != nil {
					t.Fatalf("global cap ignored: %v %v", release2 != nil, e)
				}
				p, e = d.store.LeaseSnapshot(ctx, key)
				if e != nil || len(p.Holders) != 1 || len(p.Waiting) != 0 {
					t.Fatal("cancel leaked role slot", p, e)
				}
				assertHostCounts(t, a, 1, 0)
			} else {
				if e != nil || release2 == nil {
					t.Fatalf("task scopes interfere: %v", e)
				}
				p, e = d.store.LeaseSnapshot(ctx, roleTestKey(doc, "second", "coder"))
				if e != nil || len(p.Holders) != 1 {
					t.Fatal("second task missing role", p, e)
				}
				assertHostCounts(t, a, 2, 0)
				if e = release2(); e != nil {
					t.Fatal(e)
				}
			}
			if e = release1(); e != nil {
				t.Fatal(e)
			}
			released = true
			assertHostCounts(t, a, 0, 0)
			p, e = d.store.LeaseSnapshot(ctx, key)
			if e != nil || len(p.Holders) != 0 || len(p.Waiting) != 0 {
				t.Fatal("released role retained", p, e)
			}
			// The key excludes revision; a changed cap (FAE-SCH-006: the newest
			// live revision's, here the caller's own) governs future grants of the
			// shared workflow-global pool.
			if scope == "workflow-global" {
				doc.Roles["coder"] = workflow.Role{Scope: scope, Cap: 2}
				b.Document = doc
				release, e := a.Acquire(ctx, first, b, engine.Session{ID: "changed-cap", Role: "coder"})
				if e != nil {
					t.Fatal("changed cap refused", e)
				}
				if p, e = d.store.LeaseSnapshot(ctx, key); e != nil || p.Capacity != 2 || p.AdmissionLimit != 0 {
					t.Fatal("changed cap not applied", p, e)
				}
				if e = release(); e != nil {
					t.Fatal(e)
				}
				assertHostCounts(t, a, 0, 0)
			}
		})
	}
}

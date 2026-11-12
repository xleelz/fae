package daemon

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/workflow"
)

// FAE-SCH-008: within a role's queue the longest-waiting turn is granted next,
// even after it has waited past the heavy-op maximum wait; it is never dropped
// and re-queued behind later arrivals.
func TestRoleWaiterKeepsQueuePositionPastMaxWait(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	a := admissionFixture(t, 4)
	a.daemon = d
	paneFixture(t, a, 60)
	a.inventory = func(context.Context) (map[string]int, error) { return map[string]int{}, nil }
	var skew atomic.Int64
	a.queueClock = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	doc := capsDoc("fair", 1, 1)
	jobs := map[string]controlplane.JobSnapshot{}
	for _, id := range []string{"holder", "oldest", "later"} {
		jobs[id] = viewJob(t, id, 10, doc, 1, "spec")
	}
	jobs["later"] = viewJob(t, "later", 50, doc, 1, "spec")
	v, e := newScheduleView(jobs)
	if e != nil {
		t.Fatal(e)
	}
	a.view.Store(v)
	key := roleTestKey(doc, "", "oracle")
	acquire := func(task string) func() error {
		release, e := a.Acquire(ctx, agentTurn(task), execution.Binding{Document: doc}, engine.Session{ID: task + "-oracle", Role: "oracle"})
		if e != nil {
			t.Error(task, e)
			return nil
		}
		return release
	}
	hold := acquire("holder")
	granted := make(chan string, 2)
	releases := map[string]func() error{}
	results := make(chan func() error, 2)
	wait := func(task string) {
		go func() {
			r := acquire(task)
			granted <- task
			results <- r
		}()
	}
	wait("oldest")
	awaitRoleHolders(t, d, key, 1, 1)
	// The oldest waiter has now waited past the 1800 s maximum wait.
	skew.Store(int64(31 * time.Minute))
	wait("later")
	awaitRoleHolders(t, d, key, 1, 2)
	p, e := d.store.LeaseSnapshot(ctx, key)
	if e != nil || len(p.Waiting) != 2 || !p.Waiting[0].Unbounded || !p.Waiting[1].Unbounded {
		t.Fatal("role waiters are bounded", p, e)
	}
	if e = hold(); e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"oldest", "later"} {
		select {
		case got := <-granted:
			if got != want {
				t.Fatalf("granted %s before %s", got, want)
			}
			releases[got] = <-results
			if releases[got] == nil {
				t.Fatal(got, "admission failed")
			}
			if e = releases[got](); e != nil {
				t.Fatal(e)
			}
		case <-time.After(10 * time.Second):
			t.Fatal(want, "not granted")
		}
	}
}

// FAE-SCH-008 acceptance: a task that parks again every round (all of its
// routes unavailable) never starves five ready tasks that have capacity; each
// of the five progresses in every round.
func TestPerpetualParkerDoesNotStarveReadyTasks(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.MaxInFlight = 0
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	ready := defaults.Workflow()
	for name, role := range ready.Roles {
		role.Cap = 8
		ready.Roles[name] = role
	}
	parker := defaults.Workflow()
	parker.ID = "parker"
	for name, role := range parker.Roles {
		role.Routes = []workflow.Binding{{Provider: "fixture", Model: "down"}}
		parker.Roles[name] = role
	}
	for _, doc := range []workflow.Document{ready, parker} {
		if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err = d.store.ObserveProvider(ctx, controlplane.ProviderObservation{ID: "down", TaskID: "health-fixture", Provider: "fixture", Model: "down", Outcome: providerhealth.Unavailable, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "parker", WorkflowID: "parker", Priority: 100, AuthorizingMessage: "message"}, "parker"); err != nil {
		t.Fatal(err)
	}
	var tasks []string
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("ready-%d", i)
		tasks = append(tasks, id)
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	job := func(id string) engine.Job {
		var e envelope
		if err := decodeEnvelope(d.jobs[id].Payload, &e); err != nil {
			t.Fatal(err)
		}
		return e.Job
	}
	progress := map[string]int{}
	parks := 0
	for round := 1; round <= 5; round++ {
		if err = d.round(); err != nil {
			t.Fatal(err)
		}
		// Every turn the round launched finishes before the next round.
		for deadline := time.Now().Add(3 * time.Minute); ; {
			d.mu.Lock()
			settled := len(d.results) >= len(d.active)
			d.mu.Unlock()
			if settled {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("round turns did not finish")
			}
			time.Sleep(20 * time.Millisecond)
		}
		d.mu.Lock()
		p := job("parker")
		if p.Parked && p.Reason == "all-routes-unavailable" {
			parks++
		}
		if p.Attempts != 0 || p.Sequence != 0 {
			d.mu.Unlock()
			t.Fatal("parker dispatched or charged", p.Attempts, p.Sequence)
		}
		withheld := d.withheld.Load()
		for _, id := range tasks {
			n := len(job(id).History)
			host := withheld != nil && (withheld.Tasks[id] == withheldHostGuard || withheld.Tasks[id] == withheldPaneLimit)
			if n <= progress[id] && !host {
				d.mu.Unlock()
				t.Fatalf("round %d: %s did not progress (%d events)", round, id, n)
			}
			progress[id] = n
		}
		d.mu.Unlock()
	}
	if parks < 4 {
		t.Fatal("parker did not keep parking", parks)
	}
}

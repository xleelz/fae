package daemon

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

// FAE-SCH-003: every task has an admission time, so equal priorities order by
// admission whether a task came from `fae run` or the backlog.
func TestOrderingUsesAdmissionTimeOfEveryTask(t *testing.T) {
	d := &Daemon{jobs: map[string]controlplane.JobSnapshot{}}
	now := time.Now().UTC()
	for id, at := range map[string]time.Time{"zeta-run": now, "backlog": now.Add(time.Minute), "alpha-run": now.Add(2 * time.Minute)} {
		raw, _ := json.Marshal(envelope{Admission: AdmitParams{Priority: 10}, AdmittedAt: at})
		d.jobs[id] = controlplane.JobSnapshot{Payload: raw}
	}
	graph := controlplane.BacklogGraph{Items: []controlplane.BacklogItem{{ID: "backlog", Admitted: now.Add(time.Minute)}}}
	ids, err := d.orderedTaskIDs(graph)
	if err != nil || !reflect.DeepEqual(ids, []string{"zeta-run", "backlog", "alpha-run"}) {
		t.Fatal(ids, err)
	}
}

// FAE-SCH-003 acceptance: reprioritising a pending item above a running
// sibling's priority makes it the next dispatch.
func TestReprioritizeAboveRunningSiblingIsNextDispatch(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.MaxInFlight = 0
	gated := &drainExecutor{started: make(chan string, 16), release: make(chan struct{}), delegate: engine.NewScripted(defaults.HappyScript())}
	cfg.Executor = gated
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(t, d) })
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	for id, priority := range map[string]int{"running": 20, "early": 10} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", Priority: priority, AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "boost", Tree: "tree", Priority: 10, AuthorizingMessage: "message"}, "boost"); err != nil {
		t.Fatal(err)
	}
	// Every task enters its first stage; then one task may be in flight.
	if err = d.round(); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.config.MaxInFlight = 1
	ids, _ := d.orderedTaskIDs(controlplane.BacklogGraph{})
	d.mu.Unlock()
	if !reflect.DeepEqual(ids, []string{"running", "early", "boost"}) {
		t.Fatal("equal priorities not in admission order", ids)
	}
	next := func() string {
		t.Helper()
		for i := 0; i < 10; i++ {
			if err := d.round(); err != nil {
				t.Fatal(err)
			}
			select {
			case id := <-gated.started:
				return id
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatal("no dispatch")
		return ""
	}
	if id := next(); id != "running" {
		t.Fatal("first dispatch", id)
	}
	if _, err = call(t, d, "Reprioritize", TaskControlParams{ID: "boost", Priority: 30, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	gated.release <- struct{}{}
	if id := next(); id != "boost" {
		t.Fatal("reprioritised task was not the next dispatch", id)
	}
}

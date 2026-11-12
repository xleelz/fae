package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
)

// FAE-SCH-004: tasks in flight default to -1 (unbounded); 8 ready independent
// tasks all dispatch with ample capacity.
func TestTasksInFlightDefaultUnbounded(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.MaxInFlight = 0
	gated := &drainExecutor{started: make(chan string, 16), release: make(chan struct{}), delegate: engine.NewScripted(defaults.HappyScript())}
	cfg.Executor = gated
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(t, d) })
	if d.config.MaxInFlight != -1 || d.effectiveTasksInFlight() != math.MaxInt {
		t.Fatal("default tasks in flight is bounded", d.config.MaxInFlight, d.effectiveTasksInFlight())
	}
	authorize(t, d)
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("unbounded-%d", i)
		if _, err := call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	started := map[string]bool{}
	deadline := time.After(20 * time.Second)
	for len(started) < 8 {
		select {
		case id := <-gated.started:
			started[id] = true
		case <-deadline:
			t.Fatalf("only %d of 8 ready tasks dispatched: %v", len(started), started)
		}
	}
}

func TestTasksInFlightConfigurationBounds(t *testing.T) {
	for _, n := range []int{-1, 1, 256, 257, 1000000} {
		if e := validateRuntimeConfig(RuntimeConfig{TasksInFlight: n}); e != nil {
			t.Fatal(n, e)
		}
	}
	for _, n := range []int{-2, 1000001} {
		if e := validateRuntimeConfig(RuntimeConfig{TasksInFlight: n}); e == nil {
			t.Fatal("accepted", n)
		}
	}
	for limit, want := range map[int]int{-1: math.MaxInt, 1: 1, 300: 300} {
		d := &Daemon{config: Config{MaxInFlight: limit}}
		if got := d.effectiveTasksInFlight(); got != want {
			t.Fatal(limit, got)
		}
	}
	for limit, want := range map[int]int{-1: 256, 1: 1, 4: 4, 100000: 256} {
		if got := completionBuffer(limit); got != want {
			t.Fatal(limit, got)
		}
	}
}

// FAE-SCH-010: every round records why each ready task was not dispatched,
// as counts by reason in status and a trace line when the set changes.
func TestDriveRecordsWithheldReasons(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.MaxInFlight = 1
	gated := &drainExecutor{started: make(chan string, 4), release: make(chan struct{}), delegate: engine.NewScripted(defaults.HappyScript())}
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
	items := []BacklogItemParams{}
	for _, id := range []string{"a", "b", "c"} {
		items = append(items, BacklogItemParams{AdmitParams: AdmitParams{ID: id, Tree: "tree", Priority: 10, Content: "accept " + id}, Source: "ticket/" + id})
	}
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: items, Edges: []controlplane.BacklogEdge{{From: "a", To: "c", Kind: "blocking"}}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	withheld := func() (map[string]int, map[string]string) {
		t.Helper()
		s := d.status.Load().(statusSnapshot)
		counts, _ := s.Scheduler["withheld"].(map[string]int)
		tasks, _ := s.Scheduler["withheld_tasks"].(map[string]string)
		return counts, tasks
	}
	if err = d.dispatch(); err != nil {
		t.Fatal(err)
	}
	if counts, tasks := withheld(); counts["edge"] != 1 || tasks["c"] != "edge" || len(tasks) != 1 {
		t.Fatal("first round", counts, tasks)
	}
	if err = d.dispatch(); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-gated.started:
		if id != "a" {
			t.Fatal("dispatched", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no dispatch")
	}
	if counts, tasks := withheld(); counts["tasks_in_flight"] != 1 || counts["edge"] != 1 || tasks["b"] != "tasks_in_flight" || tasks["c"] != "edge" || len(tasks) != 2 {
		t.Fatal("second round", counts, tasks)
	}
	raw, err := os.ReadFile(d.layout.Log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "drive withheld round=") || !strings.Contains(string(raw), "b:tasks_in_flight") || !strings.Contains(string(raw), "c:edge") {
		t.Fatalf("withheld trace missing:\n%s", raw)
	}
	resp, err := call(t, d, "Status", nil, "")
	if err != nil || !strings.Contains(string(resp.Result), `"withheld"`) {
		t.Fatal("status lacks withheld counts", string(resp.Result), err)
	}
}

// Admission waits and refusals classify active and parked tasks.
func TestWithheldReasonClassification(t *testing.T) {
	a := &liveAdmission{}
	a.view.Store(&scheduleView{molecule: map[string]string{"t": "m", "p": "m"}})
	a.waiting("t", withheldCap)
	if r := a.withheldReason("t"); r != withheldCap {
		t.Fatal(r)
	}
	a.waiting("t", "")
	if r := a.withheldReason("t"); r != "" {
		t.Fatal("admitted task withheld", r)
	}
	a.refused("p", withheldHostGuard, &scheduler.AdmissionRefusal{Reason: "PaneLimit"})
	if r := a.refusalReason("p"); r != withheldPaneLimit {
		t.Fatal(r)
	}
	a.refused("p", withheldCap, context.DeadlineExceeded)
	if r := a.refusalReason("p"); r != withheldCap {
		t.Fatal(r)
	}
	a.refused("maintenance", withheldHostGuard, context.DeadlineExceeded)
	if r := a.refusalReason("maintenance"); r != "" {
		t.Fatal("unscheduled work recorded", r)
	}
	for job, want := range map[string]string{
		`{"deferred":true}`: withheldDeferral,
		`{"parked":true,"reason":"all-routes-unavailable"}`:  withheldRoute,
		`{"parked":true,"reason":"ReviewPending"}`:           withheldDeferral,
		`{"parked":true,"reason":"infrastructure-deferred"}`: withheldCap,
		`{"parked":true,"reason":"MissingInput: [x]"}`:       withheldParked,
	} {
		var j engine.Job
		if err := json.Unmarshal([]byte(job), &j); err != nil {
			t.Fatal(err)
		}
		j.TaskID = "p"
		if got := parkedReason(j, a); got != want {
			t.Fatal(job, got)
		}
	}
	// FAE-RT-010: a park on routes the bridge refused, whose reason names
	// the refusals, is withheld as route_refused.
	refused := engine.Job{TaskID: "p", Parked: true, Reason: "all-routes-unavailable: every route for role spec was refused by the bridge before its provider ran: p/m: refused"}
	if got := parkedReason(refused, a); got != withheldRouteRefused {
		t.Fatal(got)
	}
}

// FAE-SCH-012: a daemon whose effective pane limit is not above the
// workflow-global slots plus one refuses to start, naming both numbers.
func TestStartRefusesPaneLimitBelowFloor(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	binary := testHerdr(t)
	cfg.Live.Adapter, cfg.Live.HerdrBinary, cfg.Live.PaneLimit = "multiplexer", binary, 7
	if _, err := Start(ctx, cfg); err == nil || !strings.Contains(err.Error(), "pane limit 7 must exceed workflow-global slots plus one (7)") {
		t.Fatal("unusable pane limit started", err)
	}
	cfg.Live.PaneLimit, cfg.Instance = 8, "floor-ok"
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stop(t, d)
}

func TestPaneFloorNamesFullFloor(t *testing.T) {
	doc := workflow.Document{Roles: map[string]workflow.Role{"a": {Scope: "workflow-global", Cap: 3}, "b": {Scope: "workflow-global", Cap: 3}, "c": {Scope: "task-bound", Cap: 50}}}
	for i := 0; i < 20; i++ {
		if e := validatePaneFloor(doc, 4); e == nil || !strings.Contains(e.Error(), "pane limit 4 must exceed workflow-global slots plus one (7)") {
			t.Fatal(e)
		}
	}
	doc.Roles["huge"] = workflow.Role{Scope: "workflow-global", Cap: math.MaxInt}
	if e := validatePaneFloor(doc, 1000000); e == nil {
		t.Fatal("overflowing floor accepted")
	}
}

// awaitLaunchedTurn waits until a live turn has passed admission and crossed
// its durable launch boundary. Agent sessions hold no heavy-op lease, so the
// journal proof, not the heavy pool, shows an admitted turn.
func awaitLaunchedTurn(t *testing.T, d *Daemon) {
	t.Helper()
	// The hang guard extends while real host pressure keeps a task parked.
	guard := newHangGuard(10 * time.Second)
	for guard.observe(anyDeferredPark(d)) {
		proofs, e := d.live.guard.journal.Entries()
		for _, p := range proofs {
			if e == nil && p.State == "launching" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, _ := os.ReadFile(d.layout.Log)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	t.Fatalf("live turn never launched under admission; log tail:\n%s", strings.Join(lines[max(0, len(lines)-40):], "\n"))
}

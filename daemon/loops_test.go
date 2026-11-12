package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/loops"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func loopRead(t *testing.T, d *Daemon, id string) loops.Schedule {
	t.Helper()
	response, e := call(t, d, "Schedule", ScheduleParams{Action: "inspect", ID: id}, "")
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(response.Result)
	var s loops.Schedule
	if e = json.Unmarshal(raw, &s); e != nil {
		t.Fatal(e)
	}
	return s
}
func loopWait(t *testing.T, d *Daemon, id, state string) loops.Schedule {
	t.Helper()
	// A live loop deferred by host admission retries after 5 s without
	// consuming an iteration; the hang guard extends while it is deferred.
	guard := newHangGuard(20 * time.Second)
	iterations := int64(-1)
	for {
		s := loopRead(t, d, id)
		if s.State == state {
			return s
		}
		if s.Iterations != iterations {
			iterations = s.Iterations
			guard.progressed()
		}
		if _, deferred := deferredRefusal(errors.New(s.Error)); !guard.observe(deferred) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("loop did not reach", state, loopRead(t, d, id))
	return loops.Schedule{}
}
func TestLoopFiveIterationsAuditedAndRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	spec := loops.Spec{ID: "five", Kind: "loop", Role: "oracle", Prompt: "remember and continue", MaxIterations: 5}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Spec: &spec}, ""); e == nil {
		t.Fatal("unauthorized create")
	}
	authorize(t, d)
	p := ScheduleParams{Action: "create", Spec: &spec, AuthorizingMessage: "message"}
	if _, e = call(t, d, "Schedule", p, ""); e != nil {
		t.Fatal(e)
	}
	s := loopWait(t, d, "five", "completed")
	if s.Iterations != 5 || s.InFlight || s.SessionID != "loop-five" {
		t.Fatal(s)
	}
	var counts struct{ Prompts int }
	json.Unmarshal(s.Context, &counts)
	if counts.Prompts != 5 {
		t.Fatal(s)
	}
	var audits int
	e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "count loops", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit WHERE action='loop iteration completed'").Scan(&audits)
	})
	if e != nil || audits != 5 {
		t.Fatal(audits, e)
	}
	jobs, e := d.store.ListJobs(ctx, 100)
	if e != nil || len(jobs) != 0 {
		t.Fatal("loop entered pipeline", jobs, e)
	}
	stop(t, d)
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	s = loopRead(t, d, "five")
	if s.Iterations != 5 || s.State != "completed" {
		t.Fatal(s)
	}
	if _, e = call(t, d, "Schedule", p, ""); e == nil {
		t.Fatal("duplicate schedule")
	}
}
func TestLoopUncertainRestartNeverReplays(t *testing.T) {
	cfg, ctx := fixture(t)
	started := make(chan struct{}, 1)
	cfg.LoopRunner = func(ctx context.Context, r loops.Request) (loops.Result, error) {
		started <- struct{}{}
		<-ctx.Done()
		return loops.Result{}, ctx.Err()
	}
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	spec := loops.Spec{ID: "uncertain", Kind: "loop", Role: "oracle", Prompt: "once"}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Spec: &spec, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("not started")
	}
	if _, err := call(t, d, "Drain", map[string]string{"authorizing_message": "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if d.status.Load().(statusSnapshot).Drained {
		t.Fatal("drain ignored active loop")
	}
	stop(t, d)
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	s := loopRead(t, d, "uncertain")
	if s.State != "uncertain" || !s.InFlight || s.Iterations != 0 {
		t.Fatal(s)
	}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "resume", ID: "uncertain", AuthorizingMessage: "message"}, ""); e == nil {
		t.Fatal("uncertain replay")
	}
	select {
	case <-started:
		t.Fatal("replayed")
	case <-time.After(50 * time.Millisecond):
	}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "retire", ID: "uncertain", AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
}
func TestLoopExpiryPauseResume(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	spec := loops.Spec{ID: "expiry", Kind: "loop", Role: "oracle", Prompt: "once", IntervalSeconds: 60, ExpiresAt: time.Now().Add(time.Second)}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Spec: &spec, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "pause", ID: "expiry", AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "resume", ID: "expiry", AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	s := loopWait(t, d, "expiry", "expired")
	if s.InFlight {
		t.Fatal(s)
	}
	// FAE-CLI-002: expires_in_seconds is counted from the daemon clock; a
	// loop states at most one of it and an explicit expires_at.
	before := time.Now().UTC()
	clocked := loops.Spec{ID: "daemon-clock", Kind: "loop", Role: "oracle", Prompt: "once", IntervalSeconds: 60}
	reply, e := call(t, d, "Schedule", ScheduleParams{Action: "create", Spec: &clocked, ExpiresInSeconds: 3600, AuthorizingMessage: "message"}, "")
	if e != nil {
		t.Fatal(e)
	}
	var created loops.Schedule
	if e = json.Unmarshal(reply.Result, &created); e != nil || created.Spec.ExpiresAt.Before(before.Add(time.Hour)) || created.Spec.ExpiresAt.After(time.Now().UTC().Add(time.Hour)) {
		t.Fatal("daemon-clock expiry", created.Spec.ExpiresAt, e)
	}
	both := loops.Spec{ID: "both", Kind: "loop", Role: "oracle", Prompt: "once", IntervalSeconds: 60, ExpiresAt: time.Now().Add(time.Hour)}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Spec: &both, ExpiresInSeconds: 60, AuthorizingMessage: "message"}, ""); e == nil || !strings.Contains(e.Error(), "at most one") {
		t.Fatal("ambiguous loop expiry accepted", e)
	}
}

func TestLoopLiveBridgeFixture(t *testing.T) {
	if os.Getenv("LOOP_DAEMON_FIXTURE") != "1" {
		return
	}
	raw, e := os.ReadFile(os.Args[len(os.Args)-1])
	if e != nil {
		os.Exit(31)
	}
	var c agentruntime.BridgeConfig
	if json.Unmarshal(raw, &c) != nil {
		os.Exit(32)
	}
	if c.SessionPolicy != "task-bound" || c.FreshContext || !strings.HasPrefix(c.WorkflowID, "fae-loop:") {
		os.Exit(33)
	}
	handoff, _ := json.Marshal(agentruntime.Handoff{DispatchID: c.DispatchID, SessionID: c.SessionID, TaskID: c.TaskID, Output: json.RawMessage(`{"message":"continued"}`)})
	if os.WriteFile(c.Handoff, handoff, 0600) != nil {
		os.Exit(34)
	}
	os.Exit(0)
}
func TestLoopLiveAdmissionAndWorktree(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	exe, _ := os.Executable()
	cfg.Live.Command = []string{exe, "-test.run=^TestLoopLiveBridgeFixture$", "--", "{config}"}
	cfg.Live.Environment = map[string]string{"LOOP_DAEMON_FIXTURE": "1"}
	cfg.Live.PersistentLoops = true
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	spec := loops.Spec{ID: "real", Kind: "loop", Role: "oracle", Prompt: "continue", MaxIterations: 2, Binding: workflow.Binding{Provider: "fixture", Model: "model"}}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Spec: &spec, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	s := loopWait(t, d, "real", "completed")
	if s.Iterations != 2 {
		t.Fatal(s)
	}
	pool, e := d.live.guard.host.Snapshot(ctx)
	if e != nil || len(pool.Holders) != 0 || len(pool.Waiting) != 0 {
		t.Fatal(pool, e)
	}
	binding := d.live.manager.Binding("provider-loops", "loop-real")
	if binding.Path == cfg.Workspace || liveGit(t, binding.Path, "rev-parse", "HEAD") != cfg.Live.Base || liveGit(t, binding.Path, "status", "--porcelain") != "" {
		t.Fatal("loop isolation")
	}
	if liveGit(t, cfg.Workspace, "status", "--porcelain") != "" {
		t.Fatal("main workspace changed")
	}
}

func TestLoopDeferralDoesNotConsumeIteration(t *testing.T) {
	cfg, ctx := fixture(t)
	var attempts atomic.Int64
	cfg.LoopRunner = func(ctx context.Context, r loops.Request) (loops.Result, error) {
		if attempts.Add(1) == 1 {
			return loops.Result{}, &loops.DeferredError{Cause: errors.New("capacity")}
		}
		if r.Iteration != 1 || r.Attempt != 2 {
			return loops.Result{}, errors.New("deferral changed iteration or reused dispatch")
		}
		return loops.Scripted(ctx, r)
	}
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	spec := loops.Spec{ID: "deferred", Kind: "loop", Role: "oracle", Prompt: "once", MaxIterations: 1}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Spec: &spec, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		s := loopRead(t, d, "deferred")
		if s.Error != "" {
			if s.State != "active" || s.InFlight || s.Iterations != 0 {
				t.Fatal(s)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := loopWait(t, d, "deferred", "completed")
	if s.Iterations != 1 || s.Dispatches != 2 || attempts.Load() != 2 {
		t.Fatal(s, attempts.Load())
	}
}

func TestLoopDoesNotStarveWorkflow(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.MaxInFlight = 1
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	spec := loops.Spec{ID: "continuous", Kind: "loop", Role: "oracle", Prompt: "continue"}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Spec: &spec, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "Admit", AdmitParams{ID: "alongside", WorkflowID: "default", Tree: "tree", AuthorizingMessage: "message"}, "alongside"); e != nil {
		t.Fatal(e)
	}
	awaitState(t, d, "alongside", "Done")
	s := loopRead(t, d, "continuous")
	if s.Iterations == 0 || s.State != "active" {
		t.Fatal("loop failed to share capacity", s)
	}
}

func TestLoopQueuedCompletionDuringShutdown(t *testing.T) {
	// Cancellation and a buffered completion may both win select. Neither should
	// attempt collection or turn ordinary shutdown into a daemon failure.
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		d := &Daemon{ctx: ctx, cancel: cancel, done: make(chan struct{}), child: &controlplane.Server{}, loopResults: make(chan loopCompletion, 1)}
		d.loopResults <- loopCompletion{id: "already-shutting-down"}
		cancel()
		d.drive()
		if d.err != nil {
			t.Fatal("cancellation reported as daemon failure", d.err)
		}
	}
}

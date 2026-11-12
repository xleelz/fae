package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

func TestStopMarkerListsInFlightAndParsesLegacy(t *testing.T) {
	if m := parseStopMarker([]byte(`true`)); !m.Graceful || !m.legacy || m.planned("a", "d") {
		t.Fatal("legacy marker", m)
	}
	if m := parseStopMarker([]byte(`false`)); m.Graceful {
		t.Fatal("crash marker read as graceful")
	}
	m := parseStopMarker([]byte(`{"graceful":true,"in_flight":[{"task":"a","dispatch":"d1","stage":"spec"}]}`))
	if !m.planned("a", "d1") || m.planned("a", "d2") || m.planned("b", "d1") {
		t.Fatal("in-flight set", m)
	}
	if parseStopMarker([]byte(`{"graceful":false,"in_flight":[{"task":"a","dispatch":"d1"}]}`)).planned("a", "d1") {
		t.Fatal("non-graceful marker authorised replay")
	}
}

// startedExecutor reports when a worker has actually begun a dispatch; a
// durable in-flight row alone does not prove the launch happened.
type startedExecutor struct {
	inner   engine.Executor
	started chan string
}

func (x startedExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	select {
	case x.started <- d.TaskID + "/" + d.Stage:
	default:
	}
	return x.inner.Execute(ctx, d)
}
func awaitStarted(t *testing.T, started chan string, want string) {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case got := <-started:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatal("never started", want)
		}
	}
}
func awaitInFlight(t *testing.T, d *Daemon, id, stage string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := d.store.GetJob(context.Background(), id); err == nil {
			var e envelope
			if json.Unmarshal(s.Payload, &e) == nil && e.Job.InFlight != nil && e.Job.InFlight.Stage == stage {
				return e.Job.InFlight.ID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never reached %s", id, stage)
	return ""
}

// FAE-ROB-010/011: a graceful stop lets an in-flight commit finish and records
// it, leaves other turns for resume, reports shutting-down during drain-down,
// lists the in-flight set, and the restart charges no recovery budget.
func TestGracefulShutdownFinishesCommitAndReplaysWithoutCharge(t *testing.T) {
	cfg, ctx := fixture(t)
	script := defaults.HappyScript()
	commit := append([]engine.ScriptStep(nil), script["commit"]...)
	commit[0].Delay = 2 * time.Second
	script["committer/commit"] = commit
	slow := append([]engine.ScriptStep(nil), script["spec"]...)
	slow[0].Delay = time.Minute
	script["slow/spec"] = slow
	started := make(chan string, 256)
	cfg.Executor = startedExecutor{engine.NewScripted(script), started}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	for _, id := range []string{"committer", "slow"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	awaitStarted(t, started, "committer/commit")
	slowDispatch := awaitInFlight(t, d, "slow", "spec")
	commitDispatch := awaitInFlight(t, d, "committer", "commit")
	closed := make(chan error, 1)
	began := time.Now()
	go func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		closed <- d.Close(closeCtx)
	}()
	shuttingDown := false
	for !shuttingDown && time.Since(began) < 1500*time.Millisecond && len(closed) == 0 {
		if r, err := call(t, d, "Status", nil, ""); err == nil {
			var status map[string]any
			json.Unmarshal(r.Result, &status)
			shuttingDown = status["shutting_down"] == true
		}
		if _, err := call(t, d, "Admit", AdmitParams{ID: "late", Tree: "tree", AuthorizingMessage: "message"}, "late"); err == nil && shuttingDown {
			t.Fatal("mutation accepted during drain-down")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !shuttingDown {
		t.Fatal("status did not report shutting-down during drain-down")
	}
	if err = <-closed; err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(began); elapsed > 20*time.Second {
		t.Fatal("drain-down not bounded", elapsed)
	}
	restartAt := time.Now()
	script["slow/spec"] = script["spec"]
	cfg.Executor = engine.NewScripted(script)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	committed := awaitState(t, d, "committer", "Done")
	var e envelope
	json.Unmarshal(committed.Payload, &e)
	found := false
	for _, p := range e.Job.Provenance {
		if p.DispatchID == commitDispatch {
			found = p.ProducedAt.Before(restartAt)
		}
	}
	if !found || e.Job.Counts["commit"] != 1 {
		t.Fatal("commit was not finished and recorded during drain-down")
	}
	replayed := awaitState(t, d, "slow", "Done")
	json.Unmarshal(replayed.Payload, &e)
	if e.Job.Failures["dispatch-recovery"] != 0 || e.Job.Attempts != 1 {
		t.Fatal("planned restart charged budget", e.Job.Failures, e.Job.Attempts)
	}
	count := 0
	for _, p := range e.Job.Provenance {
		if p.DispatchID == slowDispatch {
			count++
		}
	}
	if count != 1 {
		t.Fatal("interrupted turn not replayed exactly once", count)
	}
}

// FAE-ROB-010: an overrunning commit is cancelled at the bound, Close still
// reaps children and releases the instance, and the next start recovers.
func TestShutdownOverrunStillCleansUp(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.DrainBound = 300 * time.Millisecond
	script := defaults.HappyScript()
	commit := append([]engine.ScriptStep(nil), script["commit"]...)
	commit[0].Delay = time.Minute
	script["stuck/commit"] = commit
	cfg.Executor = engine.NewScripted(script)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "stuck", Tree: "tree", AuthorizingMessage: "message"}, "stuck"); err != nil {
		t.Fatal(err)
	}
	awaitInFlight(t, d, "stuck", "commit")
	began := time.Now()
	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = d.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(began); elapsed > 15*time.Second {
		t.Fatal("overrun not bounded", elapsed)
	}
	select {
	case <-d.child.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Dolt child not reaped after overrun")
	}
	if _, err = api.Call(context.Background(), d.layout.Socket, nil, api.Request{Version: 1, Method: "Status"}); err == nil {
		t.Fatal("listener survived Close")
	}
	script["stuck/commit"] = script["commit"]
	cfg.Executor = engine.NewScripted(script)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	awaitState(t, d, "stuck", "Done")
}

package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

type drainExecutor struct {
	started  chan string
	release  chan struct{}
	delegate *engine.Scripted
}

func (x *drainExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	x.started <- d.TaskID
	select {
	case <-ctx.Done():
		return engine.Result{}, ctx.Err()
	case <-x.release:
	}
	return x.delegate.Execute(ctx, d)
}

func TestDrainPersistsTwoActiveTurnsAndResumesAfterRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	gated := &drainExecutor{started: make(chan string, 4), release: make(chan struct{}), delegate: engine.NewScripted(defaults.HappyScript())}
	cfg.Executor = gated
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(t, d) })
	if _, err := call(t, d, "Drain", map[string]string{"authorizing_message": "absent"}, ""); err == nil {
		t.Fatal("unauthorized drain accepted")
	}
	authorize(t, d)
	for _, id := range []string{"one", "two"} {
		if _, err := call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-gated.started:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if _, err := call(t, d, "Drain", map[string]string{"authorizing_message": "message"}, ""); err != nil {
		t.Fatal(err)
	}
	s := d.status.Load().(statusSnapshot)
	if !s.Draining || s.Active != 2 {
		t.Fatalf("wrong drain snapshot %+v", s)
	}
	if _, err := call(t, d, "Admit", AdmitParams{ID: "late", Tree: "tree", AuthorizingMessage: "message"}, "late"); err == nil {
		t.Fatal("admitted during drain")
	}
	close(gated.release)
	deadline := time.Now().Add(10 * time.Second)
	for d.status.Load().(statusSnapshot).Active != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !d.status.Load().(statusSnapshot).Drained {
		t.Fatal("drain did not durably collect active turns")
	}
	saved := map[string]envelope{}
	for _, id := range []string{"one", "two"} {
		snapshot, err := d.store.GetJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var e envelope
		if err = json.Unmarshal(snapshot.Payload, &e); err != nil {
			t.Fatal(err)
		}
		if e.Job.InFlight != nil || len(e.Job.Provenance) != 1 || e.Job.State == "Done" {
			t.Fatalf("not safely between stages: %+v", e.Job)
		}
		saved[id] = e
	}
	// Explicit dispatch attempts cannot cross the same serialized drain barrier.
	if err := d.dispatch(); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-gated.started:
		t.Fatalf("started %s while draining", id)
	default:
	}
	stop(t, d)
	cfg.Executor = engine.NewScripted(defaults.HappyScript())
	resumed, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, resumed)
	if resumed.status.Load().(statusSnapshot).Draining {
		t.Fatal("restart retained transient drain")
	}
	for _, id := range []string{"one", "two"} {
		snapshot := awaitState(t, resumed, id, "Done")
		var e envelope
		if err = json.Unmarshal(snapshot.Payload, &e); err != nil {
			t.Fatal(err)
		}
		if e.Job.Provenance[0].DispatchID != saved[id].Job.Provenance[0].DispatchID || e.Job.Counts["intake"] != 1 {
			t.Fatal("completed turn replayed")
		}
	}
}

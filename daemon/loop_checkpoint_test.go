package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/loops"
)

func failLoopCheckpointStore(t *testing.T) *controlplane.Store {
	t.Helper()
	db, m, e := sqlmock.New()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		db.Close()
		if e := m.ExpectationsWereMet(); e != nil {
			t.Error(e)
		}
	})
	m.ExpectBegin().WillReturnError(errors.New("lost write acknowledgement"))
	m.ExpectQuery("SELECT payload_digest,payload_size FROM daemon_markers").WithArgs("provider-loops").WillReturnError(errors.New("read unavailable"))
	return controlplane.New(db, controlplane.Options{})
}
func stoppedLoopFixture(t *testing.T) (*Daemon, context.Context) {
	t.Helper()
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { stop(t, d) })
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	if _, e = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); e != nil {
		t.Fatal(e)
	}
	awaitDrive(t, d, false, 1)
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", AuthorizingMessage: "message", Spec: &loops.Spec{ID: "checkpoint-loop", Kind: "loop", Role: "planner", Prompt: "test", MaxIterations: 1, ExpiresAt: time.Now().UTC().Add(time.Hour)}}, ""); e != nil {
		t.Fatal(e)
	}
	return d, ctx
}
func TestLoopDispatchCheckpointSurvivesWriteAndReadFailure(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "not-committed", true: "lost-ack"}[committed], func(t *testing.T) {
			d, ctx := stoppedLoopFixture(t)
			var launches atomic.Int64
			d.mu.Lock()
			d.config.LoopRunner = func(ctx context.Context, r loops.Request) (loops.Result, error) {
				launches.Add(1)
				return loops.Scripted(ctx, r)
			}
			real := d.store
			d.store = failLoopCheckpointStore(t)
			if e := d.dispatchLoops(); e == nil {
				t.Fatal("expected unknown checkpoint outcome")
			}
			if d.loops["checkpoint-loop"].InFlight || d.loops["checkpoint-loop"].Dispatches != 0 || len(d.loopActive) != 0 || launches.Load() != 0 {
				t.Fatal("published or launched before checkpoint")
			}
			proposal := append(json.RawMessage(nil), d.loopCheckpoint.Proposed...)
			d.store = real
			if committed {
				if e := real.PutMarker(ctx, controlplane.Audit{Actor: "test", Action: "simulate committed lost ack", Outcome: "ok"}, "provider-loops", proposal); e != nil {
					t.Fatal(e)
				}
			}
			if e := d.reconcileLoopCheckpoint(ctx); e != nil {
				t.Fatal(e)
			}
			if d.loops["checkpoint-loop"].InFlight {
				t.Fatal("rearm consumed operation")
			}
			if e := d.dispatchLoops(); e != nil {
				t.Fatal(e)
			}
			d.mu.Unlock()
			var result loopCompletion
			select {
			case result = <-d.loopResults:
			case <-time.After(time.Second):
				t.Fatal("not launched")
			}
			if e := d.collectLoop(result); e != nil {
				t.Fatal(e)
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if e := d.dispatchLoops(); e != nil {
				t.Fatal(e)
			}
			if launches.Load() != 1 || d.loops[result.id].Iterations != 1 || d.loops[result.id].Dispatches != 1 || len(d.loopActive) != 0 || d.loopCheckpoint != nil {
				t.Fatal("duplicate or lost launch", launches.Load(), d.loops[result.id])
			}
		})
	}
}
func TestLoopCollectionCheckpointRetainsOwnership(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "not-committed", true: "lost-ack"}[committed], func(t *testing.T) {
			d, ctx := stoppedLoopFixture(t)
			d.mu.Lock()
			if e := d.dispatchLoops(); e != nil {
				t.Fatal(e)
			}
			d.mu.Unlock()
			var result loopCompletion
			select {
			case result = <-d.loopResults:
			case <-time.After(time.Second):
				t.Fatal("not launched")
			}
			d.mu.Lock()
			real := d.store
			d.store = failLoopCheckpointStore(t)
			d.mu.Unlock()
			if e := d.collectLoop(result); e == nil {
				t.Fatal("expected collection checkpoint failure")
			}
			d.mu.Lock()
			if !d.loops[result.id].InFlight || d.loops[result.id].Iterations != 0 || d.loopActive[result.id] == nil {
				t.Fatal("lost worker ownership before persistence")
			}
			proposal := append(json.RawMessage(nil), d.loopCheckpoint.Proposed...)
			d.store = real
			if committed {
				if e := real.PutMarker(ctx, controlplane.Audit{Actor: "test", Action: "simulate committed lost ack", Outcome: "ok"}, "provider-loops", proposal); e != nil {
					t.Fatal(e)
				}
			}
			if e := d.reconcileLoopCheckpoint(ctx); e != nil {
				t.Fatal(e)
			}
			d.mu.Unlock()
			if e := d.collectLoop(result); e != nil {
				t.Fatal(e)
			}
			raw, e := real.GetMarker(ctx, "provider-loops")
			if e != nil {
				t.Fatal(e)
			}
			a, _ := controlplane.CanonicalJSON(raw)
			b, _ := controlplane.CanonicalJSON(proposal)
			if string(a) != string(b) {
				t.Fatal("recomputed proposal instead of exact retained result")
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.loops[result.id].InFlight || d.loops[result.id].Iterations != 1 || d.loopActive[result.id] != nil || d.loopCheckpoint != nil {
				t.Fatal("collection not consumed exactly once")
			}
		})
	}
}

func TestLoopRetireLostAckBeforeQueuedCompletion(t *testing.T) {
	d, ctx := stoppedLoopFixture(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	d.mu.Lock()
	d.config.LoopRunner = func(ctx context.Context, r loops.Request) (loops.Result, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return loops.Result{}, ctx.Err()
	}
	if e := d.dispatchLoops(); e != nil {
		t.Fatal(e)
	}
	d.mu.Unlock()
	<-started
	d.mu.Lock()
	real := d.store
	d.store = failLoopCheckpointStore(t)
	_, e := d.schedule(ctx, ScheduleParams{Action: "retire", ID: "checkpoint-loop"}, controlplane.Audit{Actor: "test", Action: "retire", Outcome: "ok"})
	if e == nil {
		t.Fatal("expected retirement acknowledgement failure")
	}
	if d.loops["checkpoint-loop"].State != "active" {
		t.Fatal("published retirement before confirmation")
	}
	proposal := append(json.RawMessage(nil), d.loopCheckpoint.Proposed...)
	d.store = real
	if e = real.PutMarker(ctx, controlplane.Audit{Actor: "test", Action: "simulate lost retirement acknowledgement", Outcome: "ok"}, "provider-loops", proposal); e != nil {
		t.Fatal(e)
	}
	d.mu.Unlock()
	// Model a result which completed just before retirement. The native worker is
	// still alive so this also proves consuming retirement performs cancellation.
	result := loopCompletion{id: "checkpoint-loop", result: loops.Result{Context: json.RawMessage(`{}`), Output: json.RawMessage(`{"message":"completed"}`)}}
	if e = d.collectLoop(result); e != nil {
		t.Fatal(e)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("retired worker not cancelled")
	}
	// Discard the cancellation envelope from the synthetic worker; production has
	// one completion, represented by result above.
	<-d.loopResults
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.loops[result.id].State != "retired" || d.loops[result.id].InFlight || d.loops[result.id].Iterations != 1 || d.loopCheckpoint != nil || d.loopActive[result.id] != nil {
		t.Fatal("retire/completion checkpoint lost", d.loops[result.id])
	}
}

func TestLoopCheckpointRefusesDivergentMarker(t *testing.T) {
	d, ctx := stoppedLoopFixture(t)
	d.mu.Lock()
	defer d.mu.Unlock()
	real := d.store
	d.store = failLoopCheckpointStore(t)
	if e := d.dispatchLoops(); e == nil {
		t.Fatal("expected write failure")
	}
	d.store = real
	altered := map[string]loops.Schedule{}
	if e := json.Unmarshal(d.loopCheckpoint.Proposed, &altered); e != nil {
		t.Fatal(e)
	}
	s := altered["checkpoint-loop"]
	s.Error = "foreign checkpoint"
	altered["checkpoint-loop"] = s
	raw, _ := json.Marshal(altered)
	if e := real.PutMarker(ctx, controlplane.Audit{Actor: "test", Action: "divergent loop fixture", Outcome: "ok"}, "provider-loops", raw); e != nil {
		t.Fatal(e)
	}
	if e := d.reconcileLoopCheckpoint(ctx); e == nil {
		t.Fatal("accepted divergent loop state")
	}
	if d.loops["checkpoint-loop"].InFlight || len(d.loopActive) != 0 || d.loopCheckpoint == nil {
		t.Fatal("divergence changed ownership")
	}
}

func TestDrainRefusesUnresolvedCheckpoints(t *testing.T) {
	d, _ := stoppedLoopFixture(t)
	for _, kind := range []string{"loop", "task"} {
		d.mu.Lock()
		if kind == "loop" {
			d.loopCheckpoint = &loopCheckpoint{}
		} else {
			d.pendingCheckpoint = &jobCheckpoint{}
		}
		d.mu.Unlock()
		if _, e := call(t, d, "Drain", map[string]string{"authorizing_message": "message"}, ""); e == nil {
			t.Fatal("drained with unresolved", kind, "checkpoint")
		}
		d.mu.Lock()
		if d.draining {
			t.Fatal("failed drain changed state")
		}
		d.loopCheckpoint = nil
		d.pendingCheckpoint = nil
		d.mu.Unlock()
	}
	if _, e := call(t, d, "Drain", map[string]string{"authorizing_message": "message"}, ""); e != nil {
		t.Fatal(e)
	}
}

func TestLoopCheckpointImmediateAcknowledgementReconciliation(t *testing.T) {
	db, m, e := sqlmock.New()
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	s, e := loops.New(loops.Spec{ID: "immediate", Kind: "loop", Role: "planner", Prompt: "test", MaxIterations: 1}, time.Now().UTC())
	if e != nil {
		t.Fatal(e)
	}
	d := &Daemon{ctx: context.Background(), store: controlplane.New(db, controlplane.Options{}), loops: map[string]loops.Schedule{s.Spec.ID: s}}
	s.InFlight = true
	s.Dispatches++
	raw, _ := json.Marshal(map[string]loops.Schedule{s.Spec.ID: s})
	canonical, _ := controlplane.CanonicalJSON(raw)
	digest := controlplane.Digest(canonical)
	m.ExpectBegin().WillReturnError(errors.New("write acknowledgement lost"))
	m.ExpectQuery("SELECT payload_digest,payload_size FROM daemon_markers").WithArgs("provider-loops").WillReturnRows(sqlmock.NewRows([]string{"digest", "size"}).AddRow(digest, len(canonical)))
	m.ExpectQuery("SELECT size,chunks FROM blobs").WithArgs(digest).WillReturnRows(sqlmock.NewRows([]string{"size", "chunks"}).AddRow(len(canonical), 1))
	m.ExpectQuery("SELECT chunk_index,data FROM blob_chunks").WithArgs(digest).WillReturnRows(sqlmock.NewRows([]string{"chunk_index", "data"}).AddRow(0, canonical))
	if e = d.prepareLoopCheckpoint(d.ctx, "dispatch", s.Spec.ID, s, controlplane.Audit{Actor: "daemon", Action: "loop iteration started", Outcome: "checkpointed"}); e != nil {
		t.Fatal("confirmed checkpoint unnecessarily failed drive", e)
	}
	if d.loops[s.Spec.ID].InFlight {
		t.Fatal("proposal prematurely published")
	}
	if _, e = d.consumeLoopCheckpoint(d.ctx, "dispatch", s.Spec.ID); e != nil {
		t.Fatal(e)
	}
	if !d.loops[s.Spec.ID].InFlight || d.loopCheckpoint != nil {
		t.Fatal("exact proposal not consumed")
	}
	if e = m.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}

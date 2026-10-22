package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

type checkpointExecutor struct {
	mu       sync.Mutex
	counts   map[string]int
	delegate *engine.Scripted
}

func (x *checkpointExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	x.mu.Lock()
	x.counts[d.ID]++
	x.mu.Unlock()
	return x.delegate.Execute(ctx, d)
}

func TestDriveDelayedLostAcknowledgementReconcilesAcrossGeneration(t *testing.T) {
	for _, name := range []string{"launch", "collection", "divergent"} {
		collection := name == "collection"
		t.Run(name, func(t *testing.T) {
			cfg, ctx := fixture(t)
			executor := &checkpointExecutor{counts: map[string]int{}, delegate: engine.NewScripted(defaults.HappyScript())}
			cfg.Executor = executor
			d, e := Start(ctx, cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer stop(t, d)
			authorize(t, d)
			awaitDrive(t, d, true, 1)
			d.driveStop.Store(true)
			d.signal()
			awaitDrive(t, d, false, 1)
			if _, e = call(t, d, "Admit", AdmitParams{ID: "checkpoint", Tree: "tree", AuthorizingMessage: "message"}, "checkpoint"); e != nil {
				t.Fatal(e)
			}
			original := d.jobs["checkpoint"]
			var proposal envelope
			if e = decodeEnvelope(original.Payload, &proposal); e != nil {
				t.Fatal(e)
			}
			var dispatch *engine.Dispatch
			var events []engine.Event
			for step := 0; step < 10; step++ {
				proposal.Job, dispatch, events, e = engine.Prepare(proposal.Job)
				if e != nil {
					t.Fatal(e)
				}
				if dispatch != nil {
					break
				}
				original, e = d.persist(ctx, original, proposal, events)
				if e != nil {
					t.Fatal(e)
				}
				d.jobs[original.ID] = original
			}
			if dispatch == nil {
				t.Fatal("no dispatch prepared")
			}
			if collection {
				original, e = d.persist(ctx, original, proposal, events)
				if e != nil {
					t.Fatal(e)
				}
				d.jobs[original.ID] = original
				result, e := executor.delegate.Execute(ctx, *dispatch)
				if e != nil {
					t.Fatal(e)
				}
				proposal.Job, events, e = engine.Complete(proposal.Job, dispatch.ID, result)
				if e != nil {
					t.Fatal(e)
				}
				d.pendingCompletion = &completion{id: original.ID, dispatch: dispatch.ID, result: result}
				d.active[original.ID] = true
			}
			// Commit the exact proposal on the real plane, then model losing both its
			// acknowledgement and the immediate reconciliation read at the client seam.
			durableProposal := proposal
			if name == "divergent" {
				durableProposal.Admission.Priority++
			}
			committed, e := d.persist(ctx, original, durableProposal, events)
			if e != nil {
				t.Fatal(e)
			}
			realStore := d.store
			db, m, e := sqlmock.New()
			if e != nil {
				t.Fatal(e)
			}
			m.ExpectBegin().WillReturnError(errors.New("lost commit acknowledgement"))
			m.ExpectQuery("SELECT job_id").WillReturnError(errors.New("plane temporarily unavailable"))
			d.store = controlplane.New(db, controlplane.Options{Attempts: 1})
			_, persistErr := d.persist(ctx, original, proposal, events)
			d.store = realStore
			if e = m.ExpectationsWereMet(); e != nil {
				t.Fatal(e)
			}
			db.Close()
			if persistErr == nil || d.pendingCheckpoint == nil {
				t.Fatal("uncertain checkpoint forgotten", persistErr)
			}
			d.mu.Lock()
			d.draining = true
			e = d.requestDriveRearm(ctx, controlplane.Audit{Actor: "test", Action: "rearm", Outcome: "ok"})
			d.mu.Unlock()
			if name == "divergent" {
				if e == nil || d.pendingCheckpoint == nil || !sameJobCheckpoint(d.jobs[original.ID], original) {
					t.Fatal("divergent proposal adopted", e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			awaitDrive(t, d, true, 2)
			d.mu.Lock()
			actual := d.jobs[original.ID]
			retained := d.pendingCheckpoint != nil || d.pendingCompletion != nil || d.active[original.ID]
			d.mu.Unlock()
			if !sameJobCheckpoint(actual, committed) || retained {
				t.Fatal("exact checkpoint not reconciled", actual.Version, committed.Version, retained)
			}
			d.mu.Lock()
			d.draining = false
			d.mu.Unlock()
			d.signal()
			awaitState(t, d, "checkpoint", "Done")
			executor.mu.Lock()
			count := executor.counts[dispatch.ID]
			executor.mu.Unlock()
			want := 1
			if collection {
				want = 0
			}
			if count != want {
				t.Fatal("dispatch duplicated or lost", count, want)
			}
		})
	}
}

func TestSameJobCheckpointRequiresExactBinding(t *testing.T) {
	original := controlplane.JobSnapshot{ID: "task", Version: 2, WorkflowID: "workflow", WorkflowRevision: 1, TargetID: "target", TargetRevision: 1, State: "Pending", Payload: []byte(`{"a":1,"b":2}`)}
	canonical := original
	canonical.Payload = []byte(`{"b":2,"a":1}`)
	if !sameJobCheckpoint(original, canonical) {
		t.Fatal("equivalent JSON refused")
	}
	for _, change := range []func(*controlplane.JobSnapshot){
		func(s *controlplane.JobSnapshot) { s.Payload = []byte(`{"a":2,"b":2}`) },
		func(s *controlplane.JobSnapshot) { s.Version++ }, func(s *controlplane.JobSnapshot) { s.ID = "other" },
		func(s *controlplane.JobSnapshot) { s.WorkflowID = "other" }, func(s *controlplane.JobSnapshot) { s.WorkflowRevision++ },
		func(s *controlplane.JobSnapshot) { s.TargetID = "other" }, func(s *controlplane.JobSnapshot) { s.TargetRevision++ },
		func(s *controlplane.JobSnapshot) { s.State = "Done" },
	} {
		changed := original
		change(&changed)
		if sameJobCheckpoint(original, changed) {
			t.Fatal("changed binding accepted", changed)
		}
	}
}

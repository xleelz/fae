package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
)

func TestPaneFloorCountsOnlyGlobalSlots(t *testing.T) {
	doc := workflow.Document{Roles: map[string]workflow.Role{"global": {Scope: "workflow-global", Cap: 4}, "task": {Scope: "task-bound", Cap: 100}}}
	if e := validatePaneFloor(doc, 5); e == nil {
		t.Fatal("limit equal to floor accepted")
	}
	if e := validatePaneFloor(doc, 6); e != nil {
		t.Fatal(e)
	}
}
func TestLivePaneAdmissionRefusalCleanupAndBinding(t *testing.T) {
	for _, kind := range []string{"full", "inventory-error", "admitted"} {
		t.Run(kind, func(t *testing.T) {
			a := admissionFixture(t, 2)
			a.paneLimit = 3
			var e error
			a.owner, e = scheduler.CurrentOwner()
			if e != nil {
				t.Fatal(e)
			}
			a.birth, e = processowner.Birth(a.owner.PID)
			if e != nil {
				t.Fatal(e)
			}
			a.panes, e = scheduler.OpenPaneLedger(filepath.Join(a.daemon.layout.Root, "panes"), a.paneLimit)
			if e != nil {
				t.Fatal(e)
			}
			a.inventory = func(context.Context) (map[string]int, error) {
				if kind == "inventory-error" {
					return nil, errors.New("socket query failed")
				}
				if kind == "full" {
					return map[string]int{"/foreign": 3}, nil
				}
				return map[string]int{}, nil
			}
			release, e := a.Acquire(context.Background(), engine.Dispatch{Config: workflow.Stage{Primitive: workflow.AgentTurn}}, execution.Binding{}, engine.Session{ID: "test"})
			if kind == "admitted" {
				if e != nil {
					t.Fatal(e)
				}
				if e = a.BindPaneSocket(context.Background(), "test", "/private/pane.sock"); e != nil {
					t.Fatal(e)
				}
				rows, e := a.panes.Snapshot(context.Background())
				if e != nil || len(rows) != 1 || rows[0].Socket != "/private/pane.sock" {
					t.Fatal(rows, e)
				}
				if e = release(); e != nil {
					t.Fatal(e)
				}
			} else {
				var deferral interface{ AdmissionDeferred() bool }
				if e == nil || !errors.As(e, &deferral) || !deferral.AdmissionDeferred() {
					t.Fatal("admission failure consumed attempt", e)
				}
			}
			assertHostCounts(t, a, 0, 0)
			rows, e := a.panes.Snapshot(context.Background())
			if e != nil || len(rows) != 0 {
				t.Fatal("reservation leaked", rows, e)
			}
		})
	}
}

func TestPaneHeldUntilRetirementProof(t *testing.T) {
	a := admissionFixture(t, 2)
	ctx := context.Background()
	var e error
	a.owner, e = scheduler.CurrentOwner()
	if e != nil {
		t.Fatal(e)
	}
	a.birth, e = processowner.Birth(a.owner.PID)
	if e != nil {
		t.Fatal(e)
	}
	a.journal, e = scheduler.OpenJournal(filepath.Join(a.daemon.layout.Root, "proofs"))
	if e != nil {
		t.Fatal(e)
	}
	a.panes, e = scheduler.OpenPaneLedger(filepath.Join(a.daemon.layout.Root, "panes"), 3)
	if e != nil {
		t.Fatal(e)
	}
	a.paneLimit = 3
	a.inventory = func(context.Context) (map[string]int, error) { return map[string]int{}, nil }
	release, e := a.Acquire(ctx, engine.Dispatch{Config: workflow.Stage{Primitive: workflow.AgentTurn}}, execution.Binding{}, engine.Session{ID: "owned"})
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Launching(ctx, "owned"); e != nil {
		t.Fatal(e)
	}
	if e = release(); e == nil {
		t.Fatal("released before process retirement")
	}
	rows, e := a.panes.Snapshot(ctx)
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	// Agent sessions are not heavy ops: they hold their pane, never heavy capacity.
	assertHostCounts(t, a, 0, 0)
	if e = a.Retired(ctx, "owned", true); e != nil {
		t.Fatal(e)
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
	rows, e = a.panes.Snapshot(ctx)
	if e != nil || len(rows) != 0 {
		t.Fatal(rows, e)
	}
	assertHostCounts(t, a, 0, 0)
}

func TestDynamicPaneAdmissionUsesSharedLimitAfterIncrease(t *testing.T) {
	a := admissionFixture(t, 2)
	var err error
	a.paneLimit = 3
	a.owner, err = scheduler.CurrentOwner()
	if err != nil {
		t.Fatal(err)
	}
	a.birth, err = processowner.Birth(a.owner.PID)
	if err != nil {
		t.Fatal(err)
	}
	a.panes, err = scheduler.OpenPaneLedger(filepath.Join(a.daemon.layout.Root, "panes"), 3)
	if err != nil {
		t.Fatal(err)
	}
	a.inventory = func(context.Context) (map[string]int, error) { return map[string]int{"foreign": 3}, nil }
	ctx := context.Background()
	if err = a.panes.Configure(ctx, a.daemon.layout.Root, 3); err != nil {
		t.Fatal(err)
	}
	dispatch := engine.Dispatch{Config: workflow.Stage{Primitive: workflow.AgentTurn}}
	release, err := a.Acquire(ctx, dispatch, execution.Binding{}, engine.Session{ID: "first"})
	if err == nil || release != nil {
		t.Fatal("saturation admitted")
	}
	var deferred interface{ AdmissionDeferred() bool }
	if !errors.As(err, &deferred) || !deferred.AdmissionDeferred() {
		t.Fatal("not no-charge infrastructure deferral", err)
	}
	assertHostCounts(t, a, 0, 0)
	if err = a.panes.Configure(ctx, a.daemon.layout.Root, 4); err != nil {
		t.Fatal(err)
	}
	release, err = a.Acquire(ctx, dispatch, execution.Binding{}, engine.Session{ID: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
	assertHostCounts(t, a, 0, 0)
}

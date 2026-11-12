package scheduler

import (
	"context"
	"fmt"
	"github.com/xleelz/fae/hostguard"
	"testing"
	"time"
)

func TestWorkflowSlotsPriorityLargestFirst(t *testing.T) {
	l, o, c := slotFixture(t)
	c.Sample.Memory.Value = 10 * float64(hostguard.GiB)
	high := slotHolder("high", o)
	high.Priority = 100
	high.Memory = 8 * hostguard.GiB
	high.Ready = time.Now().Add(-time.Hour)
	d, e := l.AcquireWorkflowSlot(context.Background(), high, c)
	if e != nil || d.Granted {
		t.Fatal(d, e)
	}
	low := slotHolder("low", o)
	low.Priority = 10
	d, e = l.AcquireWorkflowSlot(context.Background(), low, c)
	if e != nil {
		t.Fatal(e)
	}
	if d.Granted {
		t.Fatalf("lower priority bypassed queue: %+v", d)
	}
}
func TestWorkflowSlotsMoleculeExemptionReevaluated(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	c.Sample.Memory.Value = 14 * float64(hostguard.GiB)
	held := slotHolder("held", o)
	held.Scope = "molecule"
	held.Workflow = "same"
	d, e := l.AcquireWorkflowSlot(ctx, held, c)
	if e != nil || !d.Granted {
		t.Fatal(d, e)
	}
	ahead := slotHolder("ahead", o)
	ahead.Scope = "molecule"
	ahead.Workflow = "same"
	ahead.MoleculeLimit = 1
	ahead.Priority = 10
	ahead.Ready = time.Now().Add(-time.Hour)
	d, e = l.AcquireWorkflowSlot(ctx, ahead, c)
	if e != nil || d.Reason != "molecule_limit" {
		t.Fatal(d, e)
	}
	behind := slotHolder("behind", o)
	behind.Instance = "other"
	behind.Priority = 10
	behind.Memory = 8 * hostguard.GiB
	d, e = l.AcquireWorkflowSlot(ctx, behind, c)
	if e != nil || d.Granted {
		t.Fatal(d, e)
	}
	if e = l.ReleaseWorkflowSlot(ctx, held.ID, o); e != nil {
		t.Fatal(e)
	}
	d, e = l.AcquireWorkflowSlot(ctx, behind, c)
	if e != nil || d.Granted {
		t.Fatal("newly eligible ahead waiter bypassed", d, e)
	}
	_, state, e := l.WorkflowSlotSnapshot(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	if !state.Waiters[ahead.ID].IgnoreFIFO {
		t.Fatal("missing stale exemption")
	}
}
func TestWorkflowSlotsSupersedingRevisionCancelsResize(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	root := slotHolder("root", o)
	root.Priority = 10
	root.Ready = time.Now().Add(-time.Hour)
	d, e := l.AcquireWorkflowSlot(ctx, root, c)
	if e != nil || !d.Granted {
		t.Fatal(d, e)
	}
	c.Sample.Memory.Value = 10 * float64(hostguard.GiB)
	large := root
	large.Revision = 2
	large.Memory = 12 * hostguard.GiB
	d, e = l.AcquireWorkflowSlot(ctx, large, c)
	if e != nil || d.Granted {
		t.Fatal(d, e)
	}
	smaller := root
	smaller.Revision = 3
	smaller.Memory = 2 * hostguard.GiB
	d, e = l.AcquireWorkflowSlot(ctx, smaller, c)
	if e != nil || !d.Granted || !d.Repriced {
		t.Fatal(d, e)
	}
	c.Sample.Memory.Value = 20 * float64(hostguard.GiB)
	other := slotHolder("other", o)
	other.Ready = time.Now()
	d, e = l.AcquireWorkflowSlot(ctx, other, c)
	if e != nil {
		t.Fatal(e)
	}
	_, state, e := l.WorkflowSlotSnapshot(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	if _, waiting := state.Waiters[root.ID]; !d.Granted || waiting {
		t.Fatal("obsolete resize survives", d, state)
	}
}

func TestWorkflowSlotsDetachedIncreaseReleaseAndOwnImpossibility(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	c.Config.PerPane = hostguard.GiB
	c.Sample.Memory.Value = 9 * float64(hostguard.GiB)
	c.ReferenceMemory = 10 * hostguard.GiB
	h := slotHolder("root", o)
	h.BaseSessions = 1
	h.Sessions = 1
	h.Panes = 0
	d, e := l.AcquireWorkflowSlot(ctx, h, c)
	if e != nil || !d.Granted {
		t.Fatal(d, e)
	}
	h.DetachedSessions = map[string]int{"child1": 1}
	d, e = l.AcquireWorkflowSlot(ctx, h, c)
	if e != nil || !d.Granted || d.GrantedHolder.Sessions != 2 {
		t.Fatal(d, e)
	}
	h.DetachedSessions = map[string]int{"child1": 1, "child2": 1}
	d, e = l.AcquireWorkflowSlot(ctx, h, c)
	if e != nil || !d.Granted || d.GrantedHolder.Sessions != 3 {
		t.Fatal(d, e)
	}
	h.DetachedSessions = map[string]int{"child1": 1, "child2": 1, "child3": 1}
	d, e = l.AcquireWorkflowSlot(ctx, h, c)
	if e != nil || d.Granted || d.Reason != "host_capacity" {
		t.Fatal(d, e)
	}
	released, e := l.ReleaseDetachedCommitments(ctx, h.ID, o, map[string]int{"child2": 1})
	if e != nil || released.Sessions != 2 {
		t.Fatal(released, e)
	}
	h.DetachedSessions = map[string]int{"child2": 1, "child3": 1}
	d, e = l.AcquireWorkflowSlot(ctx, h, c)
	if e != nil || !d.Granted || d.GrantedHolder.Sessions != 3 {
		t.Fatal(d, e)
	}
	h.DetachedSessions = map[string]int{"impossible": 100}
	d, e = l.AcquireWorkflowSlot(ctx, h, c)
	if e != nil || !d.Impossible {
		t.Fatal(d, e)
	}
	_, state, e := l.WorkflowSlotSnapshot(ctx, c)
	if e != nil || state.Holders[h.ID].Sessions != 3 {
		t.Fatal(state, e)
	}
}

func TestWorkflowSlotsDetachedWaitersKeepIndependentReadyOrder(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	c.Config.PerPane = hostguard.GiB
	c.Sample.Memory.Value = 9 * float64(hostguard.GiB)
	h := slotHolder("root", o)
	h.BaseSessions = 1
	h.Sessions = 1
	h.Panes = 0
	d, e := l.AcquireWorkflowSlot(ctx, h, c)
	if e != nil || !d.Granted {
		t.Fatal(d, e)
	}
	large := h
	large.Priority = 100
	large.WaitingTask = "first"
	large.DetachedSessions = map[string]int{"first": 3}
	d, e = l.AcquireWorkflowSlot(ctx, large, c)
	if e != nil || d.Granted {
		t.Fatal(d, e)
	}
	small := h
	small.Priority = 10
	small.WaitingTask = "second"
	small.DetachedSessions = map[string]int{"second": 1}
	d, e = l.AcquireWorkflowSlot(ctx, small, c)
	if e != nil || d.Granted {
		t.Fatal("smaller later child bypassed first", d, e)
	}
	_, state, e := l.WorkflowSlotSnapshot(ctx, c)
	if e != nil || len(state.Waiters) != 2 {
		t.Fatal("one child overwrote another waiter", state, e)
	}
}

func TestWorkflowSlotsDetachedIncreasesRaceAcrossInstances(t *testing.T) {
	for attempt := 0; attempt < 10; attempt++ {
		l, o, c := slotFixture(t)
		ctx := context.Background()
		c.Config.PerPane = hostguard.GiB
		c.Sample.Memory.Value = 10 * float64(hostguard.GiB)
		a := slotHolder("a", o)
		a.BaseSessions = 1
		a.Sessions = 1
		a.Panes = 0
		b := a
		b.ID = "b"
		b.Instance = "other"
		for _, h := range []WorkflowHolder{a, b} {
			d, e := l.AcquireWorkflowSlot(ctx, h, c)
			if e != nil || !d.Granted {
				t.Fatal(d, e)
			}
		}
		a.DetachedSessions = map[string]int{"a.child": 2}
		a.WaitingTask = "a.child"
		b.DetachedSessions = map[string]int{"b.child": 2}
		b.WaitingTask = "b.child"
		gate := make(chan struct{})
		results := make(chan WorkflowSlotDecision, 2)
		errs := make(chan error, 2)
		for _, h := range []WorkflowHolder{a, b} {
			go func(h WorkflowHolder) { <-gate; d, e := l.AcquireWorkflowSlot(ctx, h, c); results <- d; errs <- e }(h)
		}
		close(gate)
		granted := 0
		for i := 0; i < 2; i++ {
			d := <-results
			if e := <-errs; e != nil {
				t.Fatal(e)
			}
			if d.Granted {
				granted++
			}
		}
		if granted != 1 {
			t.Fatal("competing increases overcommit", granted)
		}
	}
}

func TestWorkflowSlotsDetachedReleasePreservesAcquiredBaselinePrice(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	c.Config.PerPane = 2 * hostguard.GiB
	h := slotHolder("root", o)
	h.BaseSessions = 1
	h.Sessions = 1
	h.Panes = 0
	d, e := l.AcquireWorkflowSlot(ctx, h, c)
	if e != nil || !d.Granted || d.GrantedHolder.Memory != 2*hostguard.GiB {
		t.Fatal(d, e)
	}
	c.Config.PerPane = hostguard.GiB
	h.DetachedSessions = map[string]int{"a": 1, "b": 1}
	d, e = l.AcquireWorkflowSlot(ctx, h, c)
	if e != nil || !d.Granted || d.GrantedHolder.Memory != 4*hostguard.GiB {
		t.Fatal(d, e)
	}
	remaining, e := l.ReleaseDetachedCommitments(ctx, h.ID, o, map[string]int{"b": 1})
	if e != nil || remaining.Memory != 3*hostguard.GiB {
		t.Fatal("release lost original baseline or other child", remaining, e)
	}
	remaining, e = l.ReleaseDetachedCommitments(ctx, h.ID, o, nil)
	if e != nil || remaining.Memory != 2*hostguard.GiB || remaining.Sessions != 1 {
		t.Fatal("baseline reservation changed", remaining, e)
	}
}

func TestWorkflowSlotsReferenceEstimateFormulaSurvivesSmallerAcquisitions(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	c.ReferenceMemory = 7 * hostguard.GiB
	c.Sample.Memory.Value = float64(c.Config.Soft) + 14.5*float64(hostguard.GiB)
	before, _, err := l.WorkflowSlotSnapshot(ctx, c)
	if err != nil || before.Ceiling != 2 {
		t.Fatal(before, err)
	}
	for i := 0; i < 5; i++ {
		h := slotHolder(fmt.Sprintf("small-%d", i), o)
		h.Memory = 14 * hostguard.GiB / 5
		d, e := l.AcquireWorkflowSlot(ctx, h, c)
		if e != nil || !d.Granted {
			t.Fatal("reference estimate blocked fitting holder", i, d, e)
		}
		snapshot, _, e := l.WorkflowSlotSnapshot(ctx, c)
		if e != nil {
			t.Fatal(e)
		}
		want := snapshot.Held + int(snapshot.UncommittedMemory/c.ReferenceMemory)
		if snapshot.Ceiling != want {
			t.Fatal("changed reference estimate formula", snapshot, want)
		}
	}
	sixth := slotHolder("sixth", o)
	sixth.Memory = 14 * hostguard.GiB / 5
	d, err := l.AcquireWorkflowSlot(ctx, sixth, c)
	if err != nil || d.Granted || d.Reason != "host_capacity" {
		t.Fatal(d, err)
	}
	after, _, err := l.WorkflowSlotSnapshot(ctx, c)
	if err != nil || after.Ceiling != 5 {
		t.Fatal("estimate must still include held slots", after, err)
	}
}

func TestWorkflowSlotsImpossibleRootMigrationIsNotHiddenByFundedChild(t *testing.T) {
	for _, panes := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "panes"}[panes], func(t *testing.T) {
			l, o, c := slotFixture(t)
			ctx := context.Background()
			c.Config.PerPane = hostguard.GiB
			c.TotalMemory = 10 * hostguard.GiB
			if panes {
				c.PaneAdapter = true
				c.Config.PaneLimit = 4
				c.TotalMemory = 64 * hostguard.GiB
			}
			h := slotHolder("root", o)
			h.BaseSessions = 1
			h.Sessions = 1
			h.Panes = 1
			d, e := l.AcquireWorkflowSlot(ctx, h, c)
			if e != nil || !d.Granted {
				t.Fatal(d, e)
			}
			h.DetachedSessions = map[string]int{"funded-child": 1}
			h.Panes = 2
			d, e = l.AcquireWorkflowSlot(ctx, h, c)
			if e != nil || !d.Granted {
				t.Fatal(d, e)
			}
			migration := h
			migration.Revision = 2
			migration.BaseSessions = 5
			migration.Sessions = 6
			migration.Panes = 6
			migration.WaitingTask = "root"
			d, e = l.AcquireWorkflowSlot(ctx, migration, c)
			if e != nil || !d.Impossible || d.Granted || d.Reason != "FootprintExceedsHost" {
				t.Fatal("funded child hid impossible root baseline", d, e)
			}
			_, state, e := l.WorkflowSlotSnapshot(ctx, c)
			if e != nil {
				t.Fatal(e)
			}
			held := state.Holders[h.ID]
			if held.Revision != 1 || held.Sessions != 2 || held.DetachedSessions["funded-child"] != 1 || len(state.Waiters) != 0 {
				t.Fatal("impossible migration changed existing funded work", state)
			}
		})
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
	"testing"
	"time"
)

func TestGrantedWorkflowSlotDispatchNeedsNoLedgerOrTelemetry(t *testing.T) {
	doc := slotWorkflow("cached", "tree")
	j, err := engine.New("root", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	f, err := workflow.DeriveFootprint(doc, nil, -1)
	if err != nil {
		t.Fatal(err)
	}
	j.Footprint = &f
	j.MaxSessions = -1
	j.SlotWaitReason = "host_ledger"
	j.SlotWaitSince = time.Now()
	j.Parked = true
	j.Reason = "WorkflowSlot"
	e := envelope{Job: j}
	payload, _ := json.Marshal(e)
	samples := 0
	d := &Daemon{jobs: map[string]controlplane.JobSnapshot{"root": {ID: "root", Payload: payload}}, workflowSlots: &workflowSlotManager{instance: "unit", held: map[string]scheduler.WorkflowHolder{}, wait: map[string]workflowSlotWait{}, roundLedgerFailed: true}, config: Config{WorkflowSlotSample: func(context.Context) (hostguard.Sample, error) {
		samples++
		return hostguard.Sample{}, errors.New("unreadable memory")
	}}}
	spec, err := d.slotSpec(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	d.workflowSlots.held[spec.ID] = spec
	// No ledger or plane exists in this fixture: any access would fail/panic.
	for range 30 {
		decision, err := d.ensureWorkflowSlot(context.Background(), &e)
		if err != nil || !decision.Granted || e.Job.Parked || e.Job.SlotWaitReason != "" {
			t.Fatal(decision, err, e.Job.Reason)
		}
	}
	if samples != 0 {
		t.Fatal("held dispatch sampled host capacity", samples)
	}
	if e.Job.SlotBinding == nil || e.Job.SlotBinding.Holder != spec.ID {
		t.Fatal("grant not recorded")
	}
}
func TestCachedGrantDoesNotFundNewDetachedMemberOrLargerRoot(t *testing.T) {
	held := scheduler.WorkflowHolder{ID: "holder", Workflow: "flow", Scope: "tree", Revision: 1, Sessions: 6, BaseSessions: 2, DetachedSessions: map[string]int{"root.detached.1": 4}}
	desired := held
	desired.DetachedSessions = map[string]int{"root.detached.2": 4}
	if grantedWorkflowSlotCovers(held, desired) {
		t.Fatal("old detached commitment transferred without ledger acquisition")
	}
	desired = held
	desired.BaseSessions = 3
	desired.Sessions = 7
	if grantedWorkflowSlotCovers(held, desired) {
		t.Fatal("raised root limit skipped atomic repricing")
	}
	desired = held
	desired.Revision = 2
	if grantedWorkflowSlotCovers(held, desired) {
		t.Fatal("migration skipped authoritative metadata update")
	}
	desired = held
	desired.DetachedSessions = nil
	desired.Sessions = 2
	if !grantedWorkflowSlotCovers(held, desired) {
		t.Fatal("remaining members blocked by completed child awaiting release")
	}
}

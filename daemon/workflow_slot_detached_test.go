package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetachedSubtreeRepricesInheritedHolder(t *testing.T) {
	rootDoc := slotWorkflow("cheap", "tree")
	root, e := engine.New("root", 1, rootDoc, "tree")
	if e != nil {
		t.Fatal(e)
	}
	root.State = domain.Done
	root.MaxSessions = -1
	root.Footprint = &workflow.Footprint{SlotScope: "tree", DerivedSessions: 1, EffectiveSessions: 1}
	raw, _ := json.Marshal(envelope{Job: root})
	d := &Daemon{jobs: map[string]controlplane.JobSnapshot{"root": {ID: "root", Payload: raw}}, workflowSlots: &workflowSlotManager{instance: "test", owner: scheduler.Owner{PID: 123, Started: 1}, held: map[string]scheduler.WorkflowHolder{}}}
	childDoc := slotWorkflow("expensive", "tree")
	childDoc.Signature.Inputs = nil
	childDoc.SessionCount = 0
	childDoc.Start = "discover"
	role := childDoc.Roles["author"]
	role.Cap = 8
	role.Outputs = append(role.Outputs, "List")
	childDoc.Roles["author"] = role
	childDoc.Artifacts["List"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","required":["items"],"properties":{"items":{"type":"array"}}}`)}
	childDoc.Artifacts["Joined"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"array","items":{"type":"object"}}`)}
	makeStage := childDoc.Stages["make"]
	pass := workflow.Expr{Op: "exists", Stage: "make"}
	childDoc.Stages = map[string]workflow.Stage{"discover": {Primitive: workflow.AgentTurn, Role: "author", Output: "List"}, "fan": {Primitive: workflow.FanOut, Parallel: true, Join: "join", BranchesFrom: &workflow.BranchesFromSpec{Stage: "discover", Field: "items", Min: 1, Max: 8}, BranchTemplate: &workflow.FanBranch{ID: "template", Start: "make", Pass: &pass}}, "make": makeStage, "join": {Primitive: workflow.FanIn, Output: "Joined", Rule: workflow.JoinAll, Failure: "stop"}, "stop": {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "join failed"}}
	childDoc.Edges = []workflow.Edge{{From: "discover", To: "fan"}, {From: "fan", To: "make"}, {From: "make", To: "join"}, {From: "join", To: "Done"}, {From: "join", To: "stop"}}
	actual, e := workflow.DeriveFootprint(childDoc, nil, -1)
	if e != nil || actual.DerivedSessions != 8 {
		t.Fatal(actual, e)
	}
	child, e := engine.New("root.detached.1", 1, childDoc, "tree")
	if e != nil {
		t.Fatal(e)
	}
	child.Parent = &engine.ParentRef{Task: "root", Detached: true}
	child.Detached = true
	child.TreeRoot = "root"
	child.MaxSessions = -1
	child.Footprint = &actual
	spec, e := d.slotSpec(context.Background(), envelope{Job: child})
	if e != nil {
		t.Fatal(e)
	}
	if spec.Sessions != 9 {
		t.Fatalf("detached subtree not committed with root: %+v", spec)
	}
	root.Document.SlotScope = "molecule"
	root.Footprint.SlotScope = "molecule"
	raw, _ = json.Marshal(envelope{Job: root})
	d.jobs["root"] = controlplane.JobSnapshot{ID: "root", Payload: raw}
	spec, e = d.slotSpec(context.Background(), envelope{Job: child})
	if e != nil || spec.Sessions != 2 {
		t.Fatalf("child must be derived under molecule holder scope: %+v %v", spec, e)
	}
	root.MaxSessions = 4
	root.Footprint.EffectiveSessions = 4
	raw, _ = json.Marshal(envelope{Job: root})
	d.jobs["root"] = controlplane.JobSnapshot{ID: "root", Payload: raw}
	spec, e = d.slotSpec(context.Background(), envelope{Job: child})
	if e != nil || spec.Sessions != 4 || len(spec.DetachedSessions) != 0 {
		t.Fatalf("finite tree limit already covers children: %+v %v", spec, e)
	}

}

type detachedCapacityExecutor struct {
	started  chan string
	releases map[string]chan struct{}
	delegate *engine.Scripted
}

func (x *detachedCapacityExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	x.started <- d.TaskID
	select {
	case <-ctx.Done():
		return engine.Result{}, ctx.Err()
	case <-x.releases[d.TaskID]:
	}
	return x.delegate.Execute(ctx, d)
}
func TestWorkflowSlotsNativeDetachedIncreasesReleaseForNextChild(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.MaxInFlight = 8
	cfg.WorkflowSlotSample = func(context.Context) (hostguard.Sample, error) {
		return hostguard.Sample{Memory: hostguard.Reading{Value: 8.2 * float64(hostguard.GiB), Known: true}}, nil
	}
	x := &detachedCapacityExecutor{started: make(chan string, 8), releases: map[string]chan struct{}{}, delegate: engine.NewScripted(map[string][]engine.ScriptStep{"make": {{Output: json.RawMessage(`{"text":"ok"}`)}}})}
	for _, id := range []string{"root", "root.detached.1", "root.detached.2", "root.detached.3"} {
		x.releases[id] = make(chan struct{})
	}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	doc := slotWorkflow("detached-pricing", "tree")
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "root", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "root"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-x.started:
	case <-time.After(8 * time.Second):
		t.Fatal("root not dispatched")
	}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("root.detached.%d", i)
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, WorkflowID: doc.ID, ParentTask: "root", Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	running := []string{}
	for i := 0; i < 2; i++ {
		select {
		case id := <-x.started:
			running = append(running, id)
		case <-time.After(8 * time.Second):
			t.Fatal("funded child not dispatched")
		}
	}
	select {
	case id := <-x.started:
		t.Fatal("unfunded child launched", id)
	case <-time.After(80 * time.Millisecond):
	}
	d.mu.Lock()
	capacity, _ := d.slotCapacity(ctx)
	_, state, err := d.workflowSlots.ledger.WorkflowSlotSnapshot(ctx, capacity)
	d.mu.Unlock()
	if err != nil || len(state.Holders) != 1 {
		t.Fatal(state, err)
	}
	for _, h := range state.Holders {
		if h.Sessions != 3 || len(h.DetachedSessions) != 2 {
			t.Fatal(h)
		}
	}
	close(x.releases[running[0]])
	awaitState(t, d, running[0], "Done")
	select {
	case id := <-x.started:
		if id == running[0] {
			t.Fatal("repeated child")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("completed child commitment did not fund waiter")
	}
	for id, ch := range x.releases {
		if id != running[0] {
			close(ch)
		}
	}
}

func TestNativeDetachedCycleRequiresEveryIntermediateBudget(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"make": {{Output: json.RawMessage(`{"text":"ok"}`)}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	docs := map[string]workflow.Document{}
	for _, id := range []string{"A", "B", "C", "D"} {
		doc := slotWorkflow(id, "tree")
		doc.Recursion = nil
		if id != "B" {
			doc.Recursion = &workflow.Recursion{MaxDepth: 8, MaxDescendants: 10, MaxChildrenPerVisit: 2}
		}
		docs[id] = doc
		governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: id, Document: doc, AuthorizingMessage: "message"})
	}
	id, parent := "root", ""
	for _, workflowID := range []string{"A", "B", "C", "D"} {
		if parent != "" {
			id = parent + ".detached.1"
		}
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, WorkflowID: workflowID, ParentTask: parent, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
		parent = id
	}
	_, err = call(t, d, "Admit", AdmitParams{ID: parent + ".detached.1", WorkflowID: "A", ParentTask: parent, Tree: "tree", AuthorizingMessage: "message"}, "close-cycle")
	if err == nil || !strings.Contains(err.Error(), "recursion_budget") {
		t.Fatal("unbudgeted intermediate cycle admitted", err)
	}
}

func TestCanceledDetachedWaiterReleasesFIFOPlaceAndTimerDemand(t *testing.T) {
	ctx := context.Background()
	ledger, err := scheduler.OpenWorkflowLedger(filepath.Join(t.TempDir(), "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := scheduler.CurrentOwner()
	if err != nil {
		t.Fatal(err)
	}
	capacity := scheduler.WorkflowCapacity{Config: hostguard.DefaultConfig(), Sample: hostguard.Sample{Memory: hostguard.Reading{Value: 7 * float64(hostguard.GiB), Known: true}}, TotalMemory: 64 * hostguard.GiB, ReferenceMemory: hostguard.GiB}
	h := scheduler.WorkflowHolder{ID: "holder", Instance: "instance", Workflow: "root", Revision: 1, Scope: "tree", Owner: owner, Memory: 2 * hostguard.GiB, WaitingTask: "child", Priority: 100, Ready: time.Now()}
	decision, err := ledger.AcquireWorkflowSlot(ctx, h, capacity)
	if err != nil || decision.Granted {
		t.Fatal(decision, err)
	}
	raw, _ := json.Marshal(envelope{Job: engine.Job{TaskID: "child", State: domain.Cancelled}})
	d := &Daemon{jobs: map[string]controlplane.JobSnapshot{"child": {ID: "child", Payload: raw}}, workflowSlots: &workflowSlotManager{ledger: ledger, owner: owner, wait: map[string]workflowSlotWait{"holder": {}}, waitTasks: map[string]string{"child": "holder"}}}
	if err = d.cleanupWorkflowWaitTasks(ctx); err != nil {
		t.Fatal(err)
	}
	_, state, err := ledger.WorkflowSlotSnapshot(ctx, capacity)
	if err != nil || len(state.Waiters) != 0 || len(d.workflowSlots.wait) != 0 || len(d.workflowSlots.waitTasks) != 0 {
		t.Fatal(state, err)
	}
}

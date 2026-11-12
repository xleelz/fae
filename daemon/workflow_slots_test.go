package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func slotWorkflow(id, scope string) workflow.Document {
	doc := typedTreeWorkflow(id, false)
	doc.Start = "make"
	s := doc.Stages["make"]
	s.Inputs = nil
	doc.Stages = map[string]workflow.Stage{"make": s}
	doc.Edges = []workflow.Edge{{From: "make", To: "Done"}}
	doc.Signature.Inputs = nil
	doc.SlotScope = scope
	role := doc.Roles["author"]
	role.Cap = 4
	doc.Roles["author"] = role
	return doc
}
func TestWorkflowSlotsNativeTreeAndMoleculeSharing(t *testing.T) {
	for _, scope := range []string{"tree", "molecule"} {
		t.Run(scope, func(t *testing.T) {
			cfg, ctx := fixture(t)
			cfg.MaxInFlight = 4
			cfg.WorkflowLimit = 1
			gate := &stageGate{stage: "make", reached: make(chan string, 8), release: make(chan struct{}), delegate: engine.NewScripted(map[string][]engine.ScriptStep{"make": {{Output: json.RawMessage(`{"text":"result"}`)}}})}
			cfg.Executor = gate
			d, err := Start(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer stop(t, d)
			authorize(t, d)
			doc := slotWorkflow("slot-"+scope, scope)
			governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
			for i := 0; i < 4; i++ {
				id := fmt.Sprintf("task-%d", i)
				if _, err = call(t, d, "Admit", AdmitParams{ID: id, WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
					t.Fatal(err)
				}
			}
			want := 4
			if scope == "tree" {
				want = 1
			}
			for i := 0; i < want; i++ {
				select {
				case <-gate.reached:
				case <-time.After(8 * time.Second):
					t.Fatal("expected concurrent holder session", i)
				}
			}
			if scope == "tree" {
				time.Sleep(80 * time.Millisecond)
				select {
				case id := <-gate.reached:
					t.Fatal("second tree bypassed one slot", id)
				default:
				}
				pending := jobEnvelope(t, d, ctx, "task-1")
				if pending.Job.State != "Pending" || pending.Job.Attempts != 0 || pending.Job.Sequence != 0 || pending.Job.SlotWaitReason != "workflow_limit" {
					t.Fatalf("wait changed task %+v", pending.Job)
				}
				d.mu.Lock()
				for i := 0; i < 100; i++ {
					copy := pending
					decision, e := d.ensureWorkflowSlot(ctx, &copy)
					if e != nil || decision.Granted {
						d.mu.Unlock()
						t.Fatal(decision, e)
					}
				}
				d.mu.Unlock()
				after := jobEnvelope(t, d, ctx, "task-1")
				if after.Job.Attempts != pending.Job.Attempts || after.Job.Sequence != pending.Job.Sequence {
					t.Fatal("waiting charged budget")
				}
			}
			d.mu.Lock()
			capacity, _ := d.slotCapacity(ctx)
			_, ledger, e := d.workflowSlots.ledger.WorkflowSlotSnapshot(ctx, capacity)
			d.mu.Unlock()
			if e != nil || len(ledger.Holders) != 1 {
				t.Fatal("wrong slot holder count", len(ledger.Holders), e)
			}
			close(gate.release)
			for i := 0; i < 4; i++ {
				awaitState(t, d, fmt.Sprintf("task-%d", i), "Done")
			}
		})
	}
}
func TestWorkflowSlotsNativeRecursiveChildrenReuseRootSlot(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.WorkflowLimit = 1
	cfg.MaxInFlight = 2
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"leaf": {{Output: json.RawMessage(`{"text":"recursive"}`)}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	doc := recursiveTreeWorkflow()
	doc.SlotScope = "tree"
	doc.Artifacts["Contract"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "recursive", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "recursive"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "recursive", "Done")
	root := jobEnvelope(t, d, ctx, "recursive")
	for _, id := range []string{"recursive.child.1", "recursive.child.1.child.1"} {
		child := jobEnvelope(t, d, ctx, id)
		if child.Job.State != "Done" || child.Job.SlotHolder != root.Job.SlotHolder {
			t.Fatalf("child requested own slot: %s %s vs %s", id, child.Job.SlotHolder, root.Job.SlotHolder)
		}
	}
}
func TestWorkflowLimitUnreadableLedgerDoesNotStoreRequest(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	path := filepath.Join(cfg.WorkflowLedgerPath, "state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(path, raw, 0600)
	before, beforeErr := d.store.GetMarker(ctx, "workflow-slot-request")
	if err = os.WriteFile(path, []byte("unreadable"), 0600); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	_, err = d.setWorkflowRequest(ctx, 2, controlplane.Audit{Actor: "operator", Action: "workflow-limit-set", Outcome: "ok"})
	d.mu.Unlock()
	if err == nil {
		t.Fatal("unreadable ledger accepted request")
	}
	after, afterErr := d.store.GetMarker(ctx, "workflow-slot-request")
	if string(before) != string(after) || (beforeErr == nil) != (afterErr == nil) {
		t.Fatal("failed request stored", string(before), string(after), beforeErr, afterErr)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

type workflowSlotBlocker struct {
	task     string
	started  chan struct{}
	release  chan struct{}
	delegate *engine.Scripted
}

func (x *workflowSlotBlocker) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	if d.TaskID == x.task {
		select {
		case x.started <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return engine.Result{}, ctx.Err()
		case <-x.release:
		}
	}
	return x.delegate.Execute(ctx, d)
}
func TestWorkflowSlotsNativeResumeWaitsAuditedAndContinues(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.WorkflowLimit = 1
	cfg.MaxInFlight = 2
	x := &workflowSlotBlocker{task: "blocker", started: make(chan struct{}, 1), release: make(chan struct{}), delegate: engine.NewScripted(map[string][]engine.ScriptStep{"resume/make": {{Output: json.RawMessage(`{"text":1}`)}, {Output: json.RawMessage(`{"text":2}`)}, {Output: json.RawMessage(`{"text":"fixed"}`)}}, "blocker/make": {{Output: json.RawMessage(`{"text":"blocked"}`)}}})}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	doc := slotWorkflow("resume-slots", "tree")
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	for _, id := range []string{"resume", "blocker"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
		if id == "resume" {
			awaitState(t, d, id, "Escalated")
		}
	}
	select {
	case <-x.started:
	case <-time.After(8 * time.Second):
		t.Fatal("second holder did not start")
	}
	response, err := call(t, d, "ResumeAtStage", TaskControlParams{ID: "resume", Reason: "retry supplied stage", AuthorizingMessage: "message"}, "resume-control")
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	_ = json.Unmarshal(response.Result, &reply)
	if reply["waiting"] != true || reply["withheld_reason"] != "workflow_limit" {
		t.Fatalf("resume hid slot wait: %+v", reply)
	}
	waiting := jobEnvelope(t, d, ctx, "resume")
	if waiting.Job.State != "make" || !waiting.Job.Parked || waiting.Job.Reason != "WorkflowSlot" {
		t.Fatalf("resume dispatchable without slot: %+v", waiting.Job)
	}
	close(x.release)
	awaitState(t, d, "blocker", "Done")
	awaitState(t, d, "resume", "Done")
}

func TestWorkflowSlotsNativeImpossibleFootprintDoesNotBlockSmallHolder(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.WorkflowSlotTotalMemory = 64 << 30
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"make": {{Output: json.RawMessage(`{"text":"small"}`)}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	large := slotWorkflow("large-slot", "tree")
	large.SessionCount = 200
	small := slotWorkflow("small-slot", "tree")
	for _, doc := range []workflow.Document{large, small} {
		governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
		if _, err = call(t, d, "Admit", AdmitParams{ID: doc.ID, WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, doc.ID); err != nil {
			t.Fatal(err)
		}
	}
	awaitState(t, d, large.ID, "Escalated")
	largeJob := jobEnvelope(t, d, ctx, large.ID)
	if largeJob.Job.EscalationKind != "FootprintExceedsHost" || largeJob.Job.Attempts != 0 || largeJob.Job.Sequence != 0 {
		t.Fatalf("impossible holder launched or misclassified: %+v", largeJob.Job)
	}
	awaitState(t, d, small.ID, "Done")
}

func TestWorkflowSlotsNativeDetachedUnderDoneRootReacquiresSlot(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.WorkflowLimit = 1
	x := &workflowSlotBlocker{task: "blocker", started: make(chan struct{}, 1), release: make(chan struct{}), delegate: engine.NewScripted(map[string][]engine.ScriptStep{"make": {{Output: json.RawMessage(`{"text":"result"}`)}}})}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	parentDoc := slotWorkflow("done-tree", "tree")
	childDoc := slotWorkflow("own-molecule", "molecule")
	for _, doc := range []workflow.Document{parentDoc, childDoc} {
		governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "parent", WorkflowID: parentDoc.ID, Tree: "tree", AuthorizingMessage: "message"}, "parent"); err != nil {
		t.Fatal(err)
	}
	parent := awaitState(t, d, "parent", "Done")
	if _, err = call(t, d, "Admit", AdmitParams{ID: "blocker", WorkflowID: parentDoc.ID, Tree: "tree", AuthorizingMessage: "message"}, "blocker"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-x.started:
	case <-time.After(8 * time.Second):
		t.Fatal("blocker did not acquire released slot")
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "parent.detached.1", WorkflowID: childDoc.ID, ParentTask: "parent", Tree: "tree", AuthorizingMessage: "message"}, "parent.detached.1"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		waiting := jobEnvelope(t, d, ctx, "parent.detached.1")
		if waiting.Job.SlotWaitReason == "workflow_limit" {
			if waiting.Job.State != "Pending" || waiting.Job.Sequence != 0 || waiting.Job.Attempts != 0 {
				t.Fatalf("detached dispatch bypass %+v", waiting.Job)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached child did not wait for root holder slot")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(x.release)
	awaitState(t, d, "blocker", "Done")
	awaitState(t, d, "parent.detached.1", "Done")
	child := jobEnvelope(t, d, ctx, "parent.detached.1")
	var original envelope
	_ = decodeEnvelope(parent.Payload, &original)
	if child.Job.SlotHolder != original.Job.SlotHolder {
		t.Fatal("detached used its own molecule slot")
	}
	after, err := d.store.GetJob(ctx, "parent")
	if err != nil || string(parent.Payload) != string(after.Payload) {
		t.Fatal("child changed Done parent payload", err)
	}
}

func TestWorkflowSlotsNativeMoleculeParentUsesSlotForTreeScopedChild(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.WorkflowLimit = 1
	cfg.MaxMolecules = 1
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"make": {{Output: json.RawMessage(`{"text":"child"}`)}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	child := slotWorkflow("tree-scoped-child", "tree")
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: child.ID, Document: child, AuthorizingMessage: "message"})
	parent := slotWorkflow("molecule-parent", "molecule")
	parent.Start = "consult"
	parent.Artifacts["Contract"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	parent.Stages = map[string]workflow.Stage{"consult": {Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: child.ID, Revision: 1}, Output: "Contract", OutputMap: []workflow.ArtifactMap{{Name: "contract", Output: "contract", Type: "Contract"}}}}
	parent.Edges = []workflow.Edge{{From: "consult", To: "Done"}}
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: parent.ID, Document: parent, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "molecule-root", WorkflowID: parent.ID, Tree: "tree", AuthorizingMessage: "message"}, "molecule-root"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "molecule-root", "Done")
	p := jobEnvelope(t, d, ctx, "molecule-root")
	c := jobEnvelope(t, d, ctx, "molecule-root.consult.1")
	if c.Job.SlotHolder != p.Job.SlotHolder || c.Job.State != "Done" {
		t.Fatalf("child's own tree scope took another slot: parent%s child%s state%s", p.Job.SlotHolder, c.Job.SlotHolder, c.Job.State)
	}
}

func TestWorkflowSlotsNativeLongWaitAlertsOnceAcrossEpisodeReload(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.WorkflowLimit = 1
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	cfg.WorkflowSlotNow = func() time.Time { return time.Unix(0, clock.Load()) }
	x := &workflowSlotBlocker{task: "holder", started: make(chan struct{}, 1), release: make(chan struct{}), delegate: engine.NewScripted(map[string][]engine.ScriptStep{"make": {{Output: json.RawMessage(`{"text":"result"}`)}}})}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	doc := slotWorkflow("long-wait", "tree")
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	for _, id := range []string{"holder", "waiting"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
		if id == "holder" {
			select {
			case <-x.started:
			case <-time.After(8 * time.Second):
				t.Fatal("holder not running")
			}
		}
	}
	deadline := time.Now().Add(8 * time.Second)
	var waiting envelope
	for {
		waiting = jobEnvelope(t, d, ctx, "waiting")
		if !waiting.Job.SlotWaitSince.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("wait episode not checkpointed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	clock.Add(int64(31 * time.Minute))
	d.mu.Lock()
	copy := waiting
	decision, e := d.ensureWorkflowSlot(ctx, &copy)
	if e != nil || decision.Granted {
		d.mu.Unlock()
		t.Fatal(decision, e)
	}
	d.workflowSlots.wait[copy.Job.SlotHolder] = workflowSlotWait{Reason: copy.Job.SlotWaitReason, Since: copy.Job.SlotWaitSince}
	_, e = d.ensureWorkflowSlot(ctx, &copy)
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	events, err := d.store.Events(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == "workflow-slot-wait-alert" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("long waiting episode raised%d alerts", count)
	}
	close(x.release)
	awaitState(t, d, "holder", "Done")
	awaitState(t, d, "waiting", "Done")
}

type workflowSlotExceptGate struct {
	except   string
	started  chan string
	release  chan struct{}
	delegate *engine.Scripted
}

func (x *workflowSlotExceptGate) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	if d.TaskID != x.except {
		select {
		case x.started <- d.TaskID:
		default:
		}
		select {
		case <-ctx.Done():
			return engine.Result{}, ctx.Err()
		case <-x.release:
		}
	}
	return x.delegate.Execute(ctx, d)
}
func TestWorkflowSlotsNativeRestartRestoresRunningHoldersAboveRequest(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.WorkflowLimit = 3
	cfg.MaxInFlight = 10
	script := map[string][]engine.ScriptStep{"waiting/make": {{Output: json.RawMessage(`{"text":1}`)}, {Output: json.RawMessage(`{"text":2}`)}, {Output: json.RawMessage(`{"text":"fixed"}`)}}, "make": {{Output: json.RawMessage(`{"text":"result"}`)}, {Output: json.RawMessage(`{"text":"result"}`)}, {Output: json.RawMessage(`{"text":"result"}`)}}}
	firstGate := &workflowSlotExceptGate{except: "waiting", started: make(chan string, 8), release: make(chan struct{}), delegate: engine.NewScripted(script)}
	cfg.Executor = firstGate
	first, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, first)
	authorize(t, first)
	doc := slotWorkflow("restored-slot", "tree")
	governanceCall[controlplane.Revision](t, first, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	if _, err = call(t, first, "Admit", AdmitParams{ID: "waiting", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "waiting"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, first, "waiting", "Escalated")
	for _, id := range []string{"a", "b", "c"} {
		if _, err = call(t, first, "Admit", AdmitParams{ID: id, WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		select {
		case <-firstGate.started:
		case <-time.After(8 * time.Second):
			t.Fatal("initial holder did not run")
		}
	}
	if _, err = call(t, first, "ResumeAtStage", TaskControlParams{ID: "waiting", Reason: "retry after capacity", AuthorizingMessage: "message"}, "resume-waiting"); err != nil {
		t.Fatal(err)
	}
	waitingBefore := jobEnvelope(t, first, ctx, "waiting")
	if waitingBefore.Job.Reason != "WorkflowSlot" {
		t.Fatal("test failed to create a parked stage waiter")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = first.Close(closeCtx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	otherCfg := cfg
	otherCfg.Instance = "other"
	otherCfg.WorkflowLimit = 1
	otherGate := &workflowSlotBlocker{task: "filler", started: make(chan struct{}, 1), release: make(chan struct{}), delegate: engine.NewScripted(script)}
	otherCfg.Executor = otherGate
	other, err := Start(ctx, otherCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, other)
	authorize(t, other)
	governanceCall[controlplane.Revision](t, other, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	if _, err = call(t, other, "Admit", AdmitParams{ID: "filler", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "filler"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-otherGate.started:
	case <-time.After(8 * time.Second):
		t.Fatal("other instance failed to fill freed capacity")
	}
	restartedGate := &workflowSlotExceptGate{except: "waiting", started: make(chan string, 8), release: make(chan struct{}), delegate: engine.NewScripted(script)}
	cfg.Executor = restartedGate
	cfg.WorkflowLimit = -1
	restarted, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, restarted)
	// Scripted turns lack production launch receipts and remain uncertain.
	// They still count as running holders; restoration must not wait for an
	// operator to resolve those independent execution outcomes.
	restarted.mu.Lock()
	capacity, _ := restarted.slotCapacity(ctx)
	decision, state, e := restarted.workflowSlots.ledger.WorkflowSlotSnapshot(ctx, capacity)
	request := restarted.workflowSlots.request
	restarted.mu.Unlock()
	if e != nil || decision.Held != 4 || decision.Limit != 1 || !decision.AboveLimit || len(state.Holders) != 4 || request != 3 {
		t.Fatalf("restart did not preserve exemption/request: %+v request%d %v", decision, request, e)
	}
	stillWaiting := jobEnvelope(t, restarted, ctx, "waiting")
	if stillWaiting.Job.Reason != "WorkflowSlot" || stillWaiting.Job.Sequence != waitingBefore.Job.Sequence {
		t.Fatal("restart exempted a waiter instead of a running holder")
	}
	close(otherGate.release)
	awaitState(t, other, "filler", "Done")
	close(restartedGate.release)
}

func TestWorkflowSlotsNativeTreeMigrationRepricesBeforeDispatch(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.WorkflowSlotTotalMemory = 1 << 50
	cfg.WorkflowSlotRetryInterval = 40 * time.Millisecond
	var available atomic.Int64
	available.Store(1 << 50)
	baseSample := cfg.WorkflowSlotSample
	cfg.WorkflowSlotSample = func(ctx context.Context) (hostguard.Sample, error) {
		sample, err := baseSample(ctx)
		sample.Memory = hostguard.Reading{Value: float64(available.Load()), Known: true}
		return sample, err
	}
	childGate := &workflowSlotBlocker{task: "migrating.detached.1", started: make(chan struct{}, 1), release: make(chan struct{}), delegate: engine.NewScripted(map[string][]engine.ScriptStep{"make": {{Output: json.RawMessage(`{"text":"migrated"}`)}}})}
	cfg.Executor = childGate
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	d.mu.Lock()
	capacity, err := d.slotCapacity(ctx)
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	available.Store(capacity.Config.Soft + 2*capacity.ReferenceMemory)
	doc := slotWorkflow("repriced-tree", "tree")
	doc.Start = "pause"
	doc.SessionCount = 4
	doc.Artifacts["Decision"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	doc.Stages["pause"] = workflow.Stage{Primitive: workflow.HumanDecision, Question: "continue?", Options: []workflow.DecisionOption{{ID: "go", Description: "continue", Next: "make"}}, OnTimeout: workflow.StayWaiting, Output: "Decision"}
	doc.Edges = append(doc.Edges, workflow.Edge{From: "pause", To: "make"})
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "migrating", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "migrating"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "migrating", "pause")
	childDoc := slotWorkflow("prior-child", "molecule")
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: childDoc.ID, Document: childDoc, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "migrating.detached.1", ParentTask: "migrating", WorkflowID: childDoc.ID, Tree: "tree", AuthorizingMessage: "message"}, "prior-child"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-childGate.started:
	case <-time.After(8 * time.Second):
		t.Fatal("prior child did not start")
	}

	larger := doc
	larger.SessionCount = 1000
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: larger.ID, Document: larger, AuthorizingMessage: "message"})
	response, err := call(t, d, "Migrate", MigrateParams{ID: "migrating", Revision: 2, AuthorizingMessage: "message"}, "migration")
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	_ = json.Unmarshal(response.Result, &reply)
	if reply["waiting"] != true || reply["withheld_reason"] != "host_capacity" {
		t.Fatalf("larger revision bypassed budget: %+v", reply)
	}
	waiting := jobEnvelope(t, d, ctx, "migrating")
	if waiting.Job.Footprint == nil || waiting.Job.Footprint.EffectiveSessions != 1000 || waiting.Job.Sequence != 0 || waiting.Job.Reason != "WorkflowSlot" {
		t.Fatalf("migration reused old footprint/charged work: %+v", waiting.Job)
	}
	priorChild := jobEnvelope(t, d, ctx, "migrating.detached.1")
	d.mu.Lock()
	savedCache := d.workflowSlots.held
	d.workflowSlots.held = map[string]scheduler.WorkflowHolder{}
	oldGrant, restoreErr := d.restoreSlotSpec(ctx, priorChild)
	d.workflowSlots.held = savedCache
	d.mu.Unlock()
	if restoreErr != nil || oldGrant.Sessions != 5 || oldGrant.Revision != 1 || waiting.Job.SlotBinding == nil || waiting.Job.SlotBinding.Sessions != 4 {
		t.Fatalf("ungranted migration footprint entered restart proof: %+v %v", oldGrant, restoreErr)
	}
	// No release or explicit wake follows this telemetry change. The bounded
	// waiter timer must retry, and only then may the new revision continue.
	available.Store(1 << 50)
	deadline := time.Now().Add(8 * time.Second)
	for {
		ready := jobEnvelope(t, d, ctx, "migrating")
		if ready.Job.Reason == engine.ParkHumanDecision {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("capacity retry did not resume migrated holder")
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.mu.Lock()
	capacity, _ = d.slotCapacity(ctx)
	_, state, err := d.workflowSlots.ledger.WorkflowSlotSnapshot(ctx, capacity)
	d.mu.Unlock()
	if err != nil || len(state.Holders) != 1 {
		t.Fatal(state, err)
	}
	for _, holder := range state.Holders {
		if holder.Memory < 1000*capacity.Config.PerPane || holder.Revision != 2 {
			t.Fatalf("new graph lacks committed footprint: %+v", holder)
		}
	}
	d.mu.Lock()
	savedCache = d.workflowSlots.held
	d.workflowSlots.held = map[string]scheduler.WorkflowHolder{}
	newGrant, restoreErr := d.restoreSlotSpec(ctx, priorChild)
	d.workflowSlots.held = savedCache
	d.mu.Unlock()
	if restoreErr != nil || newGrant.Sessions != 1001 || newGrant.Revision != 2 {
		t.Fatalf("accepted ancestor grant missing from restart proof: %+v %v", newGrant, restoreErr)
	}
	close(childGate.release)
	awaitState(t, d, "migrating.detached.1", "Done")
	if _, err = call(t, d, "Decide", DecideParams{ID: "migrating", Option: "go", AuthorizingMessage: "message"}, "decision"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "migrating", "Done")
}

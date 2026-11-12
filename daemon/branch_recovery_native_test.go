package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

type branchRetryRecorder struct {
	mu         sync.Mutex
	dispatches []engine.Dispatch
	delegate   *engine.Scripted
}

func (x *branchRetryRecorder) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	x.mu.Lock()
	x.dispatches = append(x.dispatches, d)
	x.mu.Unlock()
	return x.delegate.Execute(ctx, d)
}
func TestNativeBranchSchemaRetriesStayLocal(t *testing.T) {
	cfg, ctx := fixture(t)
	recorder := &branchRetryRecorder{delegate: engine.NewScripted(map[string][]engine.ScriptStep{"discover": {{Output: json.RawMessage(`{"items":[0,1,2,3]}`)}}, "work": {{Output: json.RawMessage(`{"text":0}`)}, {Output: json.RawMessage(`{"text":"ok"}`)}}})}
	cfg.Executor = recorder
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	doc := typedTreeWorkflow("branch-local-native", false)
	doc.Start = "discover"
	doc.Signature.Inputs = nil
	role := doc.Roles["author"]
	role.Cap = 4
	role.Outputs = append(role.Outputs, "List")
	doc.Roles["author"] = role
	doc.Artifacts["List"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","required":["items"],"properties":{"items":{"type":"array","items":{"type":"integer"}}}}`)}
	doc.Artifacts["Joined"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"array","items":{"type":"object"}}`)}
	retries := 1
	doc.Stages = map[string]workflow.Stage{"discover": {Primitive: workflow.AgentTurn, Role: "author", Output: "List"}, "fan": {Primitive: workflow.FanOut, BranchFailure: "fail-branch", Parallel: true, Join: "join", BranchesFrom: &workflow.BranchesFromSpec{Stage: "discover", Field: "items", Min: 1, Max: 4}, BranchTemplate: &workflow.FanBranch{ID: "template", Start: "work", Output: "Contract"}}, "work": {Primitive: workflow.AgentTurn, Role: "author", Output: "Contract", SchemaRetries: &retries}, "join": {Primitive: workflow.FanIn, Output: "Joined", Rule: workflow.JoinSettle, Failure: "stop"}, "stop": {Primitive: workflow.Escalate, Kind: "HumanDecisionRequired", Reason: "join failed"}}
	doc.Edges = []workflow.Edge{{From: "discover", To: "fan"}, {From: "fan", To: "work"}, {From: "work", To: "join"}, {From: "join", To: "Done"}, {From: "join", To: "stop"}}
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "root", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "root"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "root", "Done")
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	visits := map[string][]int{}
	for _, dispatch := range recorder.dispatches {
		if dispatch.Stage != "work" {
			continue
		}
		visits[dispatch.BranchID] = append(visits[dispatch.BranchID], dispatch.Visit)
		if dispatch.Visit == 1 && len(dispatch.PriorRejection) != 0 || dispatch.Visit == 2 && len(dispatch.PriorRejection) == 0 {
			t.Fatal("rejection crossed branch boundary", dispatch)
		}
	}
	if len(visits) != 4 {
		t.Fatal(visits)
	}
	for branch, got := range visits {
		if len(got) != 2 || got[0] != 1 || got[1] != 2 {
			t.Fatal(branch, got)
		}
	}
	saved := jobEnvelope(t, d, ctx, "root")
	if saved.Job.Attempts != 5 {
		t.Fatal("branch width charged attempts", saved.Job.Attempts)
	}
}

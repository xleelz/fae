package daemon

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func TestRuleScenarioNativeDetachedAndOfflineTreeMatch(t *testing.T) {
	cfg, ctx := fixture(t)
	root := typedTreeWorkflow("scenario-root", false)
	root.Start = "make"
	root.Stages = map[string]workflow.Stage{"make": root.Stages["make"]}
	root.Edges = []workflow.Edge{{From: "make", To: "Done"}}
	root.Signature.Inputs = nil
	stage := root.Stages["make"]
	stage.Inputs = nil
	root.Stages["make"] = stage
	child := root
	child.ID = "scenario-child"
	plan := workflow.Scenario{Seed: 42, Workflows: map[string]workflow.Document{child.ID: child, child.ID + "@2": child}, Rules: []workflow.ScenarioRule{{Stage: "make", TaskGlob: "root", Output: json.RawMessage(`{"text":"literal parent"}`), Detached: []workflow.ScenarioAdmission{{Workflow: child.ID, Revision: 2}}}}}
	cfg.Executor = engine.NewScenarioScripted(plan)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: root.ID, Document: root, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "root", WorkflowID: root.ID, Tree: "tree", AuthorizingMessage: "message"}, "root"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "root", "Done")
	awaitState(t, d, "root.detached.1", "Done")
	snap, err := d.store.GetJob(ctx, "root.detached.1")
	if err != nil {
		t.Fatal(err)
	}
	var state envelope
	if err = decodeEnvelope(snap.Payload, &state); err != nil {
		t.Fatal(err)
	}
	report, err := engine.SimulateScenario(root, "root", plan, workflow.SimulationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Tree) != 2 || state.Job.Revision != 2 || !state.Job.Detached || report.Tree[1].Task != state.Job.TaskID {
		t.Fatalf("tree diverged %+v %+v", report.Tree, state.Job.Parent)
	}
	var native map[string]any
	for _, a := range state.Job.Evidence {
		if a.Stage == "make" {
			native = a.Fields
		}
	}
	var offline map[string]any
	for _, a := range report.Tree[1].Evidence {
		if a.Stage == "make" {
			offline = a.Fields
		}
	}
	n, _ := json.Marshal(native)
	o, _ := json.Marshal(offline)
	if string(n) != string(o) {
		t.Fatalf("seed output diverged native%s offline%s", n, o)
	}
	// The callback is retry safe even if delivery repeats after the role ended.
	if err = d.scenarioActions(ctx, engine.Dispatch{ID: "root/nonexistent", TaskID: "root"}, workflow.ScenarioRule{}); err != nil {
		t.Fatal(err)
	}

}

func TestRuleScenarioNativeDynamicFanRunsRealChildTasks(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.MaxInFlight = 2
	child := typedTreeWorkflow("scenario-fan-child", false)
	child.Start = "make"
	stage := child.Stages["make"]
	stage.Inputs = nil
	child.Stages = map[string]workflow.Stage{"make": stage}
	child.Edges = []workflow.Edge{{From: "make", To: "Done"}}
	child.Signature.Inputs = nil
	parent := child
	parent.ID = "scenario-fan-parent"
	parent.Start = "discover"
	parent.Signature = nil
	parent.Artifacts = map[string]workflow.ArtifactType{"Contract": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}, "List": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["items"],"properties":{"items":{"type":"array"}}}`)}}
	parent.Artifacts["Joined"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"array","items":{"type":"object"}}`)}
	parent.Roles = map[string]workflow.Role{}
	role := child.Roles["author"]
	role.Outputs = []string{"List"}
	parent.Roles["author"] = role
	pass := workflow.Expr{Op: "exists", Stage: "consult"}
	parent.Stages = map[string]workflow.Stage{"discover": {Primitive: workflow.AgentTurn, Role: "author", Output: "List"}, "fan": {Primitive: workflow.FanOut, Join: "join", Parallel: true, BranchesFrom: &workflow.BranchesFromSpec{Stage: "discover", Field: "items", Min: 1, Max: 3}, BranchTemplate: &workflow.FanBranch{ID: "template", Start: "consult", Pass: &pass}}, "consult": {Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: child.ID, Revision: 1}, Output: "Contract", OutputMap: []workflow.ArtifactMap{{Name: "contract", Output: "contract", Type: "Contract"}}}, "join": {Primitive: workflow.FanIn, Output: "Joined", Rule: workflow.JoinAll, Failure: "stop"}, "stop": {Primitive: workflow.Escalate, Kind: "HumanDecisionRequired", Reason: "join failed"}}
	parent.Edges = []workflow.Edge{{From: "discover", To: "fan"}, {From: "fan", To: "consult"}, {From: "consult", To: "join"}, {From: "join", To: "Done"}, {From: "join", To: "stop"}}
	plan := workflow.Scenario{Seed: 9, Workflows: map[string]workflow.Document{child.ID: child}, Rules: []workflow.ScenarioRule{{Stage: "discover", Output: json.RawMessage(`{"items":[{"id":"one"},{"id":"two"}]}`)}}}
	cfg.Executor = engine.NewScenarioScripted(plan)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: parent.ID, Document: parent, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "fan-root", WorkflowID: parent.ID, Tree: "tree", AuthorizingMessage: "message"}, "fan-root"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		snap, _ := d.store.GetJob(ctx, "fan-root")
		if snap.State == "Done" {
			break
		}
		if time.Now().After(deadline) {
			rows, _ := d.store.ListJobs(ctx, 100)
			for _, r := range rows {
				var e envelope
				_ = decodeEnvelope(r.Payload, &e)
				t.Logf("task%s state%s reason%s child%+v fan%+v history%+v", r.ID, e.Job.State, e.Job.Reason, e.Job.Child, e.Job.Fan, e.Job.History)
			}
			d.mu.Lock()
			t.Logf("quarantine%v", d.quarantined)
			d.mu.Unlock()
			t.Fatal("dynamic tree stalled")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("fan-root.consult.1.%d", i)
		snap, err := d.store.GetJob(ctx, id)
		if err != nil || snap.State != "Done" {
			t.Fatalf("missing actual branch child%s: %v %+v", id, err, snap)
		}
		var e envelope
		_ = decodeEnvelope(snap.Payload, &e)
		if e.Job.Parent == nil || e.Job.Parent.BranchID != fmt.Sprintf("branch-%d", i) {
			t.Fatalf("wrong branch linkage%s: %+v", id, e.Job.Parent)
		}
	}
}

func TestRuleScenarioNativePerItemDecision(t *testing.T) {
	cfg, ctx := fixture(t)
	doc := typedTreeWorkflow("scenario-items", false)
	doc.Start = "make"
	doc.Signature = nil
	doc.Artifacts = map[string]workflow.ArtifactType{"List": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["items"],"properties":{"items":{"type":"array"}}}`)}, "Decision": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["items","decided_by","authorizing_message"]}`)}}
	role := doc.Roles["author"]
	role.Outputs = []string{"List"}
	doc.Roles["author"] = role
	projection := workflow.Projection{Stage: "make", Field: "items"}
	doc.Stages = map[string]workflow.Stage{"make": {Primitive: workflow.AgentTurn, Role: "author", Output: "List"}, "choose": {Primitive: workflow.HumanDecision, Question: "accept each item?", ItemsFrom: &projection, ItemOptions: []workflow.DecisionOption{{ID: "accept", Description: "accept item"}}, Output: "Decision", OnTimeout: workflow.StayWaiting}}
	doc.Edges = []workflow.Edge{{From: "make", To: "choose"}, {From: "choose", To: "Done"}}
	plan := workflow.Scenario{Rules: []workflow.ScenarioRule{{Stage: "make", Output: json.RawMessage(`{"items":[{"id":"a"},{"id":"b"}]}`)}, {Stage: "choose", Items: []workflow.ItemDecision{{ID: "a", Option: "accept", Rationale: "checked a"}, {ID: "b", Option: "accept", Rationale: "checked b"}}}}}
	cfg.Executor = engine.NewScenarioScripted(plan)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "items-root", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "items-root"); err != nil {
		t.Fatal(err)
	}
	snap := awaitState(t, d, "items-root", "Done")
	var e envelope
	_ = decodeEnvelope(snap.Payload, &e)
	found := false
	for _, a := range e.Job.Evidence {
		if a.Stage == "choose" {
			items, ok := a.Fields["items"].([]any)
			found = ok && len(items) == 2
		}
	}
	if !found || e.Job.Sequence != 1 {
		t.Fatalf("scripted decision failed: sequence%d evidence%+v", e.Job.Sequence, e.Job.Evidence)
	}
}

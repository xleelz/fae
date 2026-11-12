package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
	"time"
)

func nestedStatsFixture() workflow.Document {
	return workflow.Document{SchemaVersion: 2, ID: "nested", Start: "outer", TargetRequirements: &workflow.TargetRequirements{}, Limits: workflow.Limits{Attempts: 20, TestRuns: 1, ScopeWidens: 1}, Roles: map[string]workflow.Role{"author": {Instructions: "x", Briefs: map[string]string{"default": "x"}, Routes: []workflow.Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 4, Outputs: []string{"Note"}}}, Artifacts: map[string]workflow.ArtifactType{"Note": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}, "Joined": {Version: 1, Schema: json.RawMessage(`{"type":"array"}`)}}, Stages: map[string]workflow.Stage{"outer": {Primitive: workflow.FanOut, Parallel: true, Join: "outerjoin", Branches: []workflow.FanBranch{{ID: "deep", Start: "inner", Output: "Joined"}, {ID: "side", Start: "sibling"}}}, "inner": {Primitive: workflow.FanOut, Parallel: true, Join: "innerjoin", Branches: []workflow.FanBranch{{ID: "a", Start: "a"}, {ID: "b", Start: "b"}}}, "a": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"}, "b": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"}, "sibling": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"}, "innerjoin": {Primitive: workflow.FanIn, Rule: workflow.JoinAll, Output: "Joined", Failure: "stop"}, "outerjoin": {Primitive: workflow.FanIn, Rule: workflow.JoinAll, Output: "Joined", Failure: "stop"}, "stop": {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "stop"}}, Edges: []workflow.Edge{{From: "outer", To: "inner"}, {From: "outer", To: "sibling"}, {From: "inner", To: "a"}, {From: "inner", To: "b"}, {From: "a", To: "innerjoin"}, {From: "b", To: "innerjoin"}, {From: "innerjoin", To: "outerjoin"}, {From: "innerjoin", To: "stop"}, {From: "sibling", To: "outerjoin"}, {From: "outerjoin", To: "Done"}, {From: "outerjoin", To: "stop"}}}
}
func TestTreeCapabilitiesAndBacklogCeilingNative(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.RecursionCeiling = 12
	script := engine.NewScripted(map[string][]engine.ScriptStep{"a": {{Output: json.RawMessage(`{}`)}}, "b": {{Output: json.RawMessage(`{}`)}}, "sibling": {{Output: json.RawMessage(`{}`)}}, "draft": {{Output: json.RawMessage(`{"text":"ceiling"}`)}}})
	cfg.Executor = funcExecutor(func(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
		r, e := script.Execute(ctx, d)
		if e != nil {
			return r, e
		}
		cost := 1.
		stat := domain.NewTurnStat(&domain.ProviderSessionStats{CostUSD: &cost}, time.Now().Add(-time.Second), time.Now())
		stat.Task = d.TaskID
		stat.Stage = d.Stage
		stat.FanStage = d.FanStage
		stat.Branch = d.BranchID
		stat.Dispatch = d.ID
		stat.Session = d.Sessions[0].ID
		stat.Visit = d.Visit
		r.TurnStats = []domain.TurnStat{stat}
		return r, nil
	})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	nested := nestedStatsFixture()
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: nested.ID, Document: nested, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "nested-stats", WorkflowID: nested.ID, Tree: "tree", AuthorizingMessage: "message"}, "nested-stats"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "nested-stats", "Done")
	response, err := call(t, d, "Tree", TreeParams{Task: "nested-stats", Stats: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	var page TreePage
	_ = json.Unmarshal(response.Result, &page)
	if page.Stats.Stages["outer"] == nil || page.Stats.Stages["outer"].Sessions != 3 || *page.Stats.Stages["outer"].Cost.Value != 3 || page.Stats.StageDetails["inner"] == nil || page.Stats.StageDetails["inner"].Sessions != 2 || page.Stats.Sessions != 3 || page.Nodes[0].Branches["outer"] != 2 {
		t.Fatal(string(response.Result))
	}
	budget := childWorkflow()
	budget.ID = "budget-refusal"
	budget.Start = "call"
	budget.SlotScope = "tree"
	budget.MaxSessions = 1
	budget.Recursion = &workflow.Recursion{MaxDepth: 1, MaxDescendants: 1, MaxChildrenPerVisit: 1}
	budget.Artifacts["Contract"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	budget.Stages = map[string]workflow.Stage{"call": {Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: "self"}, Output: "Contract", Failure: "stop", OutputMap: []workflow.ArtifactMap{{Name: "contract", Stage: "call", Type: "Contract"}}}, "stop": {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "refused"}}
	budget.Edges = []workflow.Edge{{From: "call", To: "Done"}, {From: "call", To: "stop"}}
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: budget.ID, Document: budget, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "budget-task", WorkflowID: budget.ID, Tree: "tree", AuthorizingMessage: "message"}, "budget-task"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "budget-task", "Escalated")
	response, err = call(t, d, "Tree", TreeParams{Task: "budget-task"}, "")
	if err != nil {
		t.Fatal(err)
	}
	page = TreePage{}
	_ = json.Unmarshal(response.Result, &page)
	if len(page.Nodes) != 1 || len(page.Nodes[0].RecursionRefusals) == 0 {
		t.Fatal("actual recursion refusal missing", string(response.Result))
	}
	ceiling := childWorkflow()
	ceiling.ID = "ceiling-nine"
	ceiling.Stages = map[string]workflow.Stage{"draft": ceiling.Stages["draft"]}
	ceiling.Edges = []workflow.Edge{{From: "draft", To: "Done"}}
	ceiling.Recursion = &workflow.Recursion{MaxDepth: 9, MaxDescendants: 1, MaxChildrenPerVisit: 1}
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: ceiling.ID, Document: ceiling, AuthorizingMessage: "message"})
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{{AdmitParams: AdmitParams{ID: "ceiling-task", WorkflowID: ceiling.ID, Tree: "tree", Priority: 10, Content: "ceiling regression"}}}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal("configured ceiling lost on backlog path", err)
	}
	awaitState(t, d, "ceiling-task", "Done")
}

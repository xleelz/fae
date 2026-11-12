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

func childAliasFixture() (workflow.Document, workflow.Document) {
	parent, child := explicitChildFixture("plain")
	parent.ID = "alias-parent"
	child.Signature.Outputs = map[string]workflow.SignatureOutput{"public": {Type: "Note"}}
	parent.Artifacts["Published"] = child.Artifacts["Note"]
	parent.Signature.Outputs = map[string]workflow.SignatureOutput{"public": {Type: "Published"}}
	s := parent.Stages["consult"]
	s.OutputMap = []workflow.ArtifactMap{{Name: "note", Output: "public", As: "Published"}}
	parent.Stages["consult"] = s
	return parent, child
}
func TestChildAliasPublishesParentOutputAndCatalogChecksContracts(t *testing.T) {
	parent, child := childAliasFixture()
	raw, _ := json.Marshal(parent)
	v := ValidateWorkflowCatalog(raw, map[string]workflow.Document{child.ID: child})
	if !v.Valid {
		t.Fatal(v.Issues)
	}
	result, err := engine.SimulateScenario(parent, "root", workflow.Scenario{Workflows: map[string]workflow.Document{child.ID: child}, Rules: []workflow.ScenarioRule{{Stage: "a", Output: json.RawMessage(`{}`)}, {Stage: "b", Output: json.RawMessage(`{}`)}, {Stage: "sibling", Output: json.RawMessage(`{}`)}, {Stage: "draft", Output: json.RawMessage(`{"text":"published"}`)}}}, workflow.SimulationOptions{})
	if err != nil || result.State != domain.Done {
		t.Fatal(result.State, result.Reason, err)
	}
	alias, composite := false, false
	for _, e := range result.Evidence {
		if e.Type == "Published" {
			alias = true
			if e.Stage != "consult" || e.Fields["text"] != "published" {
				t.Fatal(e)
			}
		}
		if e.Stage == "consult" && e.Type == "Note" {
			composite = true
		}
	}
	if !alias || !composite {
		t.Fatal("alias changed composite or was not recorded", result.Evidence)
	}
	child.Signature.Outputs["public"] = workflow.SignatureOutput{Type: "Decision"}
	raw, _ = json.Marshal(parent)
	if v = ValidateWorkflowCatalog(raw, map[string]workflow.Document{child.ID: child}); v.Valid {
		t.Fatal("incompatible child output accepted")
	}
	parent, child = childAliasFixture()
	s := parent.Stages["consult"]
	s.InputMap = nil
	parent.Stages["consult"] = s
	raw, _ = json.Marshal(parent)
	if v = ValidateWorkflowCatalog(raw, map[string]workflow.Document{child.ID: child}); v.Valid {
		t.Fatal("missing child signature input accepted")
	}
}

func TestAliasAndFailureReportsNative(t *testing.T) {
	cfg, ctx := fixture(t)
	script := engine.NewScripted(map[string][]engine.ScriptStep{"a": {{Output: json.RawMessage(`{}`)}}, "b": {{Output: json.RawMessage(`{}`)}}, "sibling": {{Output: json.RawMessage(`{}`)}}, "draft": {{Output: json.RawMessage(`{"text":"published"}`)}}, "make": {{Output: json.RawMessage(`{"wrong":true}`)}}, "repair": {{Output: json.RawMessage(`{"text":"repaired"}`)}}})
	cfg.Executor = funcExecutor(func(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
		r, err := script.Execute(ctx, d)
		if err != nil {
			return r, err
		}
		cost := 1.
		s := domain.NewTurnStat(&domain.ProviderSessionStats{CostUSD: &cost}, time.Now().Add(-time.Millisecond), time.Now())
		s.Task = d.TaskID
		s.Stage = d.Stage
		s.FanStage = d.FanStage
		s.Branch = d.BranchID
		s.Dispatch = d.ID
		s.Session = d.Sessions[0].ID
		s.Visit = d.Visit
		r.TurnStats = []domain.TurnStat{s}
		return r, nil
	})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	parent, child := childAliasFixture()
	for _, doc := range []workflow.Document{child, parent} {
		governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "alias-task", WorkflowID: parent.ID, Tree: "tree", AuthorizingMessage: "message"}, "alias-task"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "alias-task", "Done")
	row, err := d.store.GetJob(ctx, "alias-task")
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err = decodeEnvelope(row.Payload, &env); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range env.Job.Evidence {
		if a.Type != "Published" {
			continue
		}
		for _, p := range env.Job.Provenance {
			if p.ArtifactID == a.ID && p.Stage == "consult" && p.Session == "alias-task.consult.1" && len(p.Inputs) == 1 {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("alias lost child identity provenance")
	}
	doc := child
	doc.ID = "repairflow"
	doc.Start = "make"
	zero := 0
	doc.Signature.Inputs = nil
	doc.Stages = map[string]workflow.Stage{"make": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note", Failure: "repair", SchemaRetries: &zero}, "repair": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"}}
	doc.Edges = []workflow.Edge{{From: "make", To: "Done"}, {From: "make", To: "repair"}, {From: "repair", To: "Done"}}
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "repair-task", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "repair-task"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "repair-task", "Done")
	page, err := d.treeSnapshot(ctx, "repair-task", true)
	if err != nil {
		t.Fatal(err)
	}
	n := page.Nodes[0]
	if len(n.FailureRoutes) != 1 || n.FailureRoutes[0].HandlerPassed == nil || !*n.FailureRoutes[0].HandlerPassed {
		t.Fatal(n.FailureRoutes)
	}
	primary := n.Stats.Stages["make"]
	if primary == nil || primary.Sessions != 2 || primary.Handlers["repair"] == nil || primary.Handlers["repair"].Sessions != 1 {
		t.Fatalf("handler not attributed to primary: %+v", n.Stats)
	}
}

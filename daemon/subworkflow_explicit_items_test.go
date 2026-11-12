package daemon

import (
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
)

func explicitChildFixture(mode string) (workflow.Document, workflow.Document) {
	parent := nestedStatsFixture()
	parent.ID = "explicit-" + mode
	child := childWorkflow()
	child.ID = "explicit-leaf"
	child.Stages = map[string]workflow.Stage{"draft": child.Stages["draft"]}
	child.Edges = []workflow.Edge{{From: "draft", To: "Done"}}
	child.Signature = &workflow.Signature{Inputs: map[string]json.RawMessage{"value": json.RawMessage(`{"type":"string"}`)}}
	parent.Signature = &workflow.Signature{Inputs: map[string]json.RawMessage{"batch": json.RawMessage(`{"type":"array","maxItems":3,"items":{"type":"object"},"default":[{"name":"one"},{"name":"two"},{"name":"three"}]}`), "seed": json.RawMessage(`{"type":"string","default":"single"}`)}}
	call := workflow.Stage{Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: child.ID, Revision: 1}, Output: "Note", InputMap: []workflow.ArtifactMap{{Name: "value", Projection: &workflow.Projection{Input: "seed"}}}, OutputMap: []workflow.ArtifactMap{{Name: "note", Stage: "draft", Type: "Note"}}, Failure: "stop"}
	if mode == "multi" {
		call.ItemsFrom = &workflow.Projection{Input: "batch"}
		call.InputMap[0].Projection = &workflow.Projection{Source: "item", Field: "name"}
	}
	if mode == "named-items" {
		parent.Signature.Inputs["seed"] = json.RawMessage(`{"type":"object","default":{"items":[{"name":"one"},{"name":"two"}]}}`)
		child.Signature.Inputs["value"] = json.RawMessage(`{"type":"object"}`)
	}
	parent.Stages["consult"] = call
	for i, e := range parent.Edges {
		if e.From == "outerjoin" && e.To == "Done" {
			parent.Edges[i].To = "consult"
		}
	}
	parent.Edges = append(parent.Edges, workflow.Edge{From: "consult", To: "Done"}, workflow.Edge{From: "consult", To: "stop"})
	return parent, child
}
func assertChildInputEvidence(t *testing.T, j engine.Job, want string) {
	t.Helper()
	if j.Inputs["value"] != want {
		t.Fatalf("child %s got wrapper/wrong item: %+v", j.TaskID, j.Inputs)
	}
	for _, e := range j.Evidence {
		if e.Stage == "input:value" {
			if e.Fields["value"] != want {
				t.Fatal(e.Fields)
			}
			return
		}
	}
	t.Fatal("missing own input evidence", j.TaskID)
}
func TestExplicitSubWorkflowItemsScenario(t *testing.T) {
	for _, mode := range []string{"plain", "multi", "named-items"} {
		t.Run(mode, func(t *testing.T) {
			parent, child := explicitChildFixture(mode)
			report, err := engine.SimulateScenario(parent, "root", workflow.Scenario{Workflows: map[string]workflow.Document{child.ID: child}, Rules: []workflow.ScenarioRule{{Stage: "a", Output: json.RawMessage(`{}`)}, {Stage: "b", Output: json.RawMessage(`{}`)}, {Stage: "sibling", Output: json.RawMessage(`{}`)}, {Stage: "draft", Output: json.RawMessage(`{"text":"child"}`)}}}, workflow.SimulationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if mode == "multi" {
				want = 4
			}
			if report.State != domain.Done || len(report.Tree) != want {
				t.Fatalf("unexpected tree: %+v", report)
			}
			if mode == "multi" {
				for i, node := range report.Tree[1:] {
					expected := []string{"one", "two", "three"}[i]
					found := false
					for _, e := range node.Evidence {
						if e.Stage == "input:value" {
							found = true
							if e.Fields["value"] != expected {
								t.Fatalf("child %s: %+v", node.Task, e.Fields)
							}
						}
					}
					if !found {
						t.Fatal("missing child input evidence", node.Task)
					}
				}
			}
		})
	}
}
func TestExplicitSubWorkflowItemsNative(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"a": {{Output: json.RawMessage(`{}`)}}, "b": {{Output: json.RawMessage(`{}`)}}, "sibling": {{Output: json.RawMessage(`{}`)}}, "draft": {{Output: json.RawMessage(`{"text":"child"}`)}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	for _, mode := range []string{"plain", "multi", "named-items"} {
		parent, child := explicitChildFixture(mode)
		child.ID += "-" + mode
		callStage := parent.Stages["consult"]
		callStage.Workflow.ID = child.ID
		parent.Stages["consult"] = callStage
		for _, doc := range []workflow.Document{child, parent} {
			governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
		}
		task := "case-" + mode
		if _, err := call(t, d, "Admit", AdmitParams{ID: task, WorkflowID: parent.ID, Tree: "tree", AuthorizingMessage: "message"}, task); err != nil {
			t.Fatal(err)
		}
		awaitState(t, d, task, "Done")
		row, err := d.store.GetJob(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		var e envelope
		if err = decodeEnvelope(row.Payload, &e); err != nil {
			t.Fatal(err)
		}
		count := 1
		if mode == "multi" {
			count = 3
		}
		for i := 0; i < count; i++ {
			id := engine.ChildID(task, "consult", 1)
			if count > 1 {
				id = engine.ChildIDIndexed(task, "consult", 1, i)
			}
			row, err = d.store.GetJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err = decodeEnvelope(row.Payload, &e); err != nil {
				t.Fatal(err)
			}
			if mode == "multi" {
				assertChildInputEvidence(t, e.Job, []string{"one", "two", "three"}[i])
			} else if mode == "plain" {
				assertChildInputEvidence(t, e.Job, "single")
			}
		}
		missing := engine.ChildIDIndexed(task, "consult", 1, count)
		if _, err = d.store.GetJob(ctx, missing); err == nil {
			t.Fatal(fmt.Sprintf("unexpected implicit child %s", missing))
		}
	}
}

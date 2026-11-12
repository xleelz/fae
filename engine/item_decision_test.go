package engine_test

import (
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
)

func itemDecisionWorkflow() workflow.Document {
	d := dataWorkflow()
	d.Start = "inventory"
	d.Signature = nil
	d.Artifacts["List"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","required":["items"],"properties":{"items":{"type":"array"}}}`)}
	d.Artifacts["Decision"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","required":["items","decided_by","authorizing_message"]}`)}
	r := d.Roles["author"]
	r.Outputs = []string{"List"}
	d.Roles["author"] = r
	p := workflow.Projection{Stage: "inventory", Field: "items"}
	count := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "count", Stage: "choose", Path: []string{"items"}, Args: []workflow.Expr{{Op: "eq", Args: []workflow.Expr{{Op: "item", Path: []string{"option"}}, {Op: "literal", Value: "accept"}}}}}, {Op: "literal", Value: 5}}}
	d.Stages = map[string]workflow.Stage{"inventory": {Primitive: workflow.AgentTurn, Role: "author", Output: "List"}, "choose": {Primitive: workflow.HumanDecision, Question: "accept items?", ItemsFrom: &p, ItemOptions: []workflow.DecisionOption{{ID: "accept", Description: "accept item"}, {ID: "reject", Description: "reject item"}}, OnTimeout: workflow.StayWaiting, Output: "Decision"}, "gate": {Primitive: workflow.Gate, Name: "five accepted items", Expr: &count, Failure: "stop"}, "stop": d.Stages["stop"]}
	d.Edges = []workflow.Edge{{From: "inventory", To: "choose"}, {From: "choose", To: "gate"}, {From: "gate", To: "Done"}, {From: "gate", To: "stop"}}
	return d
}
func TestPerItemDecisionRequiresEveryChoiceAndSimulatorMatches(t *testing.T) {
	doc := itemDecisionWorkflow()
	j := newPrimitiveJob(t, doc)
	items := []any{}
	choices := []workflow.ItemDecision{}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("part-%d", i)
		items = append(items, map[string]any{"id": id})
		choices = append(choices, workflow.ItemDecision{ID: id, Option: "accept", Rationale: "checked"})
	}
	output, _ := json.Marshal(map[string]any{"items": items})
	j = drive(t, j, engine.NewScripted(map[string][]engine.ScriptStep{"inventory": {{Output: output}}}), true)
	if _, ok := engine.PendingDecision(j); !ok {
		t.Fatal("decision not pending", j.State, j.Reason)
	}
	if _, _, _, err := engine.Decide(j, engine.Decision{Items: choices[:4], Message: "m"}); err == nil || !strings.Contains(err.Error(), "item 4") {
		t.Fatal("partial decision accepted", err)
	}
	next, _, rec, err := engine.Decide(j, engine.Decision{Items: choices, Message: "m", Actor: "operator"})
	if err != nil || len(rec.Items) != 5 {
		t.Fatal("complete choice refused", err)
	}
	done := drive(t, next, engine.NewScripted(nil), true)
	if done.State != domain.Done {
		t.Fatal("accepted item count gate failed", done.State, done.Reason)
	}
	ev := workflow.Evidence{ID: "inventory-record", TaskID: "sim", Type: "List", Version: 1, Stage: "inventory", Tree: "simulated-tree", Role: "author", Session: "sim", Provenance: true, Fields: map[string]any{"items": items}}
	sim, err := engine.Simulate(doc, "sim", workflow.Script{"inventory": {{Evidence: []workflow.Evidence{ev}}}, "choose": {{Items: choices}}}, workflow.SimulationOptions{})
	if err != nil || sim.State != domain.Done {
		t.Fatal("per-item simulator failed", err, sim.State, sim.Reason)
	}
}

package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
	"time"
)

func TestDispatchPresenceIsBoundBeforeProvider(t *testing.T) {
	doc := scenarioLeafDoc()
	doc.Signature.Inputs = map[string]json.RawMessage{"contract": json.RawMessage(`{"type":["object","null"],"properties":{"component":{"type":"string"}},"required":["component"],"default":null}`)}
	stage := doc.Stages["make"]
	stage.Brief = "{{#present input.contract}}CHILD {{input.contract.component?}}{{/present}}{{#absent input.contract}}ROOT{{/absent}}"
	doc.Stages["make"] = stage
	j, err := engine.New("root", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	j, err = engine.BindTaskInputs(j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		var d *engine.Dispatch
		j, d, _, err = engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		if d != nil {
			if present, known := d.TemplatePresence["input.contract"]; !known || present {
				t.Fatal("presence not pinned on dispatch", d.TemplatePresence)
			}
			return
		}
	}
	t.Fatal("no dispatch")
}

type roundTemplateExecutor struct{ argv [][]string }

func (x *roundTemplateExecutor) Execute(_ context.Context, d engine.Dispatch) (engine.Result, error) {
	x.argv = append(x.argv, d.Argv)
	return engine.Result{ProducedAt: time.Now().UTC(), ExecutionKind: d.ExecutionKind, DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, Output: json.RawMessage(fmt.Sprintf(`{"text":"ok","checked":1,"iteration":%v,"exit_code":0,"timed_out":false,"truncated":false}`, d.TemplateValues["loop.round.iteration"]))}, nil
}
func TestLoopIterationAndVisitBindAcrossThreeActualVisits(t *testing.T) {
	doc := scenarioLeafDoc()
	doc.Signature = nil
	doc.Start = "round"
	done := bin("and", bin("ge", workflow.Expr{Op: "field", Stage: "check", Path: []string{"checked"}}, lit(1)), bin("eq", workflow.Expr{Op: "field", Stage: "check", Path: []string{"iteration"}}, lit(3)))
	argv := []string{"checker", "out/r{{loop.round.iteration}}.json", "--visit={{visit}}"}
	doc.TargetRequirements = &workflow.TargetRequirements{Commands: [][]string{argv}}
	doc.Stages = map[string]workflow.Stage{"round": {Primitive: workflow.Loop, MaxIterations: 3, ExitGate: "done"}, "check": {Primitive: workflow.HostCommand, Weight: 1, TimeoutSeconds: 10, Output: "Note", Command: argv, Check: &workflow.CheckSpec{Format: "json", CountField: "checked"}}, "done": {Primitive: workflow.Gate, Name: "third round", Expr: &done, Failure: "round"}}
	doc.Edges = []workflow.Edge{{From: "round", To: "check"}, {From: "check", To: "done"}, {From: "done", To: "Done"}, {From: "done", To: "round"}}
	j, err := engine.New("root", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	x := &roundTemplateExecutor{}
	j = drive(t, j, x, true)
	if j.State != domain.Done || len(x.argv) != 3 {
		t.Fatal(j.State, j.Reason, x.argv)
	}
	for i, args := range x.argv {
		if args[1] != fmt.Sprintf("out/r%d.json", i+1) || args[2] != fmt.Sprintf("--visit=%d", i+1) {
			t.Fatal(args)
		}
	}
}
func TestSiblingReferencesBindPerActualFanInstance(t *testing.T) {
	doc := scenarioLeafDoc()
	doc.Start = "fan"
	doc.Signature = &workflow.Signature{Inputs: map[string]json.RawMessage{"parts": json.RawMessage(`{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]},"default":[{"id":"a"},{"id":"b"},{"id":"c"}]}`)}}
	role := doc.Roles["author"]
	role.Reads = &workflow.ReadPolicy{Deny: []string{"notes/{{siblings.id}}.md"}}
	doc.Roles["author"] = role
	doc.Stages["fan"] = workflow.Stage{Primitive: workflow.FanOut, Parallel: true, Join: "join", BranchesFrom: &workflow.BranchesFromSpec{Input: "parts", Min: 1, Max: 3}, BranchTemplate: &workflow.FanBranch{ID: "template", Start: "make"}}
	doc.Stages["join"] = workflow.Stage{Primitive: workflow.FanIn, Rule: workflow.JoinAll, Failure: "stop"}
	doc.Stages["stop"] = workflow.Stage{Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "failed"}
	doc.Edges = []workflow.Edge{{From: "fan", To: "make"}, {From: "make", To: "join"}, {From: "join", To: "Done"}, {From: "join", To: "stop"}}
	j, err := engine.New("root", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	j, err = engine.BindTaskInputs(j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		var ds []*engine.Dispatch
		j, ds, _, err = engine.PrepareParallelFanOut(j, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(ds) > 0 {
			if len(ds) != 3 {
				t.Fatal(len(ds))
			}
			for _, d := range ds {
				if len(d.BranchSiblings) != 2 {
					t.Fatal(d.BranchSiblings)
				}
				for _, other := range d.BranchSiblings {
					if other.(map[string]any)["id"] == d.BranchItem.(map[string]any)["id"] {
						t.Fatal("own item treated as sibling")
					}
				}
			}
			return
		}
		j, _, _, err = engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("no fan dispatches")
}

package engine_test

import (
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
)

func dataWorkflow() workflow.Document {
	check := bin("and", bin("eq", fieldOf("input:profile", "enabled"), lit(true)), bin("lt", fieldOf("tree", "depth"), lit(2)))
	return workflow.Document{SchemaVersion: 2, ID: "data", Start: "check", Roles: map[string]workflow.Role{"author": {Instructions: "record", Briefs: map[string]string{"default": "record"}, Routes: []workflow.Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"Note"}}}, Artifacts: map[string]workflow.ArtifactType{"Note": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}}}`)}}, Stages: map[string]workflow.Stage{"check": {Primitive: workflow.Gate, Name: "input and tree gate", Expr: &check, Failure: "stop"}, "make": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note", OperatorMaySupply: true}, "stop": {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "depth rejected"}}, Edges: []workflow.Edge{{From: "check", To: "make"}, {From: "check", To: "stop"}, {From: "make", To: "Done"}}, Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 3}, TargetRequirements: &workflow.TargetRequirements{}, Signature: &workflow.Signature{Inputs: map[string]json.RawMessage{"profile": json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"],"default":{"enabled":true}}`)}, Outputs: map[string]workflow.SignatureOutput{"note": {Type: "Note"}}}}
}
func TestPinnedInputsAndTreeGateRecordsSurviveRestart(t *testing.T) {
	doc := dataWorkflow()
	for _, depth := range []int{1, 2} {
		j, err := engine.New("task", 1, doc, "tree")
		if err != nil {
			t.Fatal(err)
		}
		j, err = engine.BindTaskInputs(j, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		j.TreeDepth = depth
		if depth == 2 {
			j.Parent = &engine.ParentRef{Task: "parent"}
			j.TreeRoot = "parent"
		}
		j = drive(t, j, engine.NewScripted(map[string][]engine.ScriptStep{"make": step(`{"text":"ok"}`)}), true)
		if depth == 1 && j.State != domain.Done || depth == 2 && j.State != domain.Escalated {
			t.Fatal("wrong tree branch", depth, j.State, j.Reason)
		}
		if err = engine.ReplayGates(j); err != nil {
			t.Fatal("tree/input gate cannot replay", err)
		}
		if depth == 2 && j.Sequence != 0 {
			t.Fatal("depth gate dispatched work", j.Sequence)
		}
	}
}
func TestOperatorSuppliedArtifactSkipsTurnAndPreservesSchemaFloor(t *testing.T) {
	doc := dataWorkflow()
	doc.Start = "make"
	doc.Stages = map[string]workflow.Stage{"make": doc.Stages["make"]}
	doc.Edges = []workflow.Edge{{From: "make", To: "Done"}}
	j, err := engine.New("task", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	j, err = engine.BindTaskInputs(j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = engine.SupplyArtifact(j, "make", json.RawMessage(`{"text":3}`), "m", "operator"); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatal("invalid supplied artifact accepted", err)
	}
	done, _, err := engine.SupplyArtifact(j, "make", json.RawMessage(`{"text":"human"}`), "m", "operator")
	if err != nil || done.State != domain.Done || done.Sequence != 0 {
		t.Fatal("operator supply failed", err, done.State, done.Sequence)
	}
	found := false
	for _, p := range done.Provenance {
		found = found || p.Stage == "make" && p.Role == "operator"
	}
	if !found {
		t.Fatal("operator provenance missing")
	}
	doc.Stages["make"] = workflow.Stage{Primitive: workflow.AgentTurn, Role: "author", Output: "Note"}
	j, err = engine.New("other", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = engine.SupplyArtifact(j, "make", json.RawMessage(`{"text":"human"}`), "m", "operator"); err == nil {
		t.Fatal("unflagged stage accepted supply")
	}
}
func TestHierarchicalChildIDsNeverBecomeHashes(t *testing.T) {
	id := strings.Repeat("r", 60)
	for depth := 2; depth <= 8; depth++ {
		parent := id
		id = engine.ChildIDIndexed(parent, strings.Repeat("s", 60), 1, 2)
		if !strings.HasPrefix(id, parent+".") {
			t.Fatal("hierarchy replaced", depth, id)
		}
	}
}

func TestBranchSubWorkflowIdentityRejectsSiblingResult(t *testing.T) {
	parent := newPrimitiveJob(t, subDoc(false))
	parent.BranchContext = &engine.BranchContext{Path: "outer/1"}
	parent = drive(t, parent, engine.NewScripted(floorScript(1)), true)
	ref, ok := engine.PendingChild(parent)
	if !ok || !strings.HasSuffix(ref.Task, ".consult.1.outer.1") {
		t.Fatalf("branch hierarchy missing: %+v %s", ref, parent.Reason)
	}
	child := childJob(t, ref, parent.TaskID, true)
	child.Parent.BranchID = "outer/0"
	if _, _, err := engine.BindChild(parent, child); err == nil {
		t.Fatal("sibling branch substituted child result")
	}
	child.Parent.BranchID = "outer/1"
	next, _, err := engine.BindChild(parent, child)
	if err != nil || next.Child != nil {
		t.Fatalf("own branch child rejected: %v", err)
	}
	if engine.Obsolete(child, &parent) {
		t.Fatal("active matching branch child treated obsolete")
	}
}

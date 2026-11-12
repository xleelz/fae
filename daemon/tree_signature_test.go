package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func typedTreeWorkflow(id string, child bool) workflow.Document {
	enabled := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "input:profile", Path: []string{"enabled"}}, {Op: "literal", Value: true}}}
	d := workflow.Document{SchemaVersion: 2, ID: id, Start: "inputgate", Roles: map[string]workflow.Role{"author": {Instructions: "record", Briefs: map[string]string{"default": "record"}, Routes: []workflow.Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"Contract"}}}, Artifacts: map[string]workflow.ArtifactType{"Contract": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string"}},"additionalProperties":false}`)}}, Stages: map[string]workflow.Stage{"inputgate": {Primitive: workflow.Gate, Name: "typed input", Expr: &enabled, Failure: "stop"}, "make": {Primitive: workflow.AgentTurn, Role: "author", Output: "Contract", Inputs: []workflow.Input{{Name: "scope", Type: "input:scope", Stage: "input:scope", Version: 1}}}, "stop": {Primitive: workflow.Escalate, Kind: "HumanDecisionRequired", Reason: "input failed"}}, Edges: []workflow.Edge{{From: "inputgate", To: "make"}, {From: "inputgate", To: "stop"}, {From: "make", To: "Done"}}, Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 3}, TargetRequirements: &workflow.TargetRequirements{}, Recursion: &workflow.Recursion{MaxDepth: 3, MaxDescendants: 5, MaxChildrenPerVisit: 2}, Signature: &workflow.Signature{Inputs: map[string]json.RawMessage{"scope": json.RawMessage(`{"type":"string"}`), "profile": json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"required":["enabled"],"default":{"enabled":true}}`)}, Outputs: map[string]workflow.SignatureOutput{"contract": {Type: "Contract"}}}}
	if child {
		d.Signature.Inputs["contract"] = json.RawMessage(`{"type":"object","required":["text"]}`)
	}
	return d
}
func TestTypedInputsDetachedDoneParentAndAggregatedRefusals(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"make": {{Output: json.RawMessage(`{"text":"durable contract"}`)}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	for _, doc := range []workflow.Document{typedTreeWorkflow("typed-root", false), typedTreeWorkflow("typed-child", true)} {
		governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "root", WorkflowID: "typed-root", Tree: "tree", Inputs: map[string]any{"scope": "src/a"}, MaxSessions: 3, AuthorizingMessage: "message"}, "root"); err != nil {
		t.Fatal(err)
	}
	before := awaitState(t, d, "root", "Done")
	var parent envelope
	if err = decodeEnvelope(before.Payload, &parent); err != nil {
		t.Fatal(err)
	}
	if parent.Job.Inputs["profile"].(map[string]any)["enabled"] != true {
		t.Fatal("input default missing", parent.Job.Inputs)
	}
	if _, err = call(t, d, "Admit", AdmitParams{WorkflowID: "typed-child", Tree: "tree", ParentTask: "root", FromParent: map[string]string{"contract": "missing"}, AuthorizingMessage: "message"}, "invalid-child"); err == nil || !strings.Contains(err.Error(), "inputs.scope") || !strings.Contains(err.Error(), "inputs.contract") || !strings.Contains(err.Error(), "from_parent.contract") {
		t.Fatal("missing aggregated refusal", err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{WorkflowID: "typed-child", Tree: "tree", ParentTask: "root", FromParent: map[string]string{"scope": "scope", "contract": "contract"}, AuthorizingMessage: "message"}, "detached"); err != nil {
		t.Fatal(err)
	}
	child := awaitState(t, d, "root.detached.1", "Done")
	var c envelope
	if err = decodeEnvelope(child.Payload, &c); err != nil {
		t.Fatal(err)
	}
	if !c.Job.Detached || c.Job.Parent.Task != "root" || c.Job.TreeRoot != "root" || c.Job.TreeDepth != 2 || c.Job.MaxSessions != 3 {
		t.Fatal("child metadata", c.Job.Parent, c.Job.TreeDepth, c.Job.TreeRoot)
	}
	for name, ids := range c.Job.InputSources {
		if name != "profile" && len(ids) == 0 {
			t.Fatal("parent provenance missing", name)
		}
	}
	after, err := d.store.GetJob(ctx, "root")
	if err != nil || !bytes.Equal(before.Payload, after.Payload) {
		t.Fatal("detached child changed done parent", err)
	}
	// A depth-3 grandchild is valid; depth 4 refuses without changing either parent.
	if _, err = call(t, d, "Admit", AdmitParams{WorkflowID: "typed-root", Tree: "tree", ParentTask: child.ID, FromParent: map[string]string{"scope": "scope"}, AuthorizingMessage: "message"}, "grandchild"); err != nil {
		t.Fatal(err)
	}
	grand := awaitState(t, d, "root.detached.1.detached.1", "Done")
	if _, err = call(t, d, "Admit", AdmitParams{WorkflowID: "typed-root", Tree: "tree", ParentTask: grand.ID, FromParent: map[string]string{"scope": "scope"}, AuthorizingMessage: "message"}, "too-deep"); err == nil || !strings.Contains(err.Error(), "recursion_budget") {
		t.Fatal("depth budget bypass", err)
	}
	// Existing children, including the detached branch and its grandchild,
	// exhaust the same root's total descendant budget.
	for n := 0; n < 3; n++ {
		_, err = call(t, d, "Admit", AdmitParams{WorkflowID: "typed-root", Tree: "tree", ParentTask: "root", FromParent: map[string]string{"scope": "scope"}, AuthorizingMessage: "message"}, fmt.Sprintf("sibling-%d", n))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = call(t, d, "Admit", AdmitParams{WorkflowID: "typed-root", Tree: "tree", ParentTask: "root", FromParent: map[string]string{"scope": "scope"}, AuthorizingMessage: "message"}, "sixth-descendant"); err == nil || !strings.Contains(err.Error(), "max_descendants") {
		t.Fatal("total descendant budget bypass", err)
	}
	parentEvents, err := d.store.JobEvents(ctx, "root", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, event := range parentEvents {
		linked = linked || event.Kind == "child-admitted"
	}
	if !linked {
		t.Fatal("parent history missing child link")
	}

}

func recursiveTreeWorkflow() workflow.Document {
	d := typedTreeWorkflow("recursive-data", false)
	d.Signature.Inputs = nil
	d.Signature.Outputs = map[string]workflow.SignatureOutput{"note": {Type: "Contract"}}
	d.Start = "child"
	d.Stages = map[string]workflow.Stage{"child": {Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: "self"}, Output: "Contract", OutputMap: []workflow.ArtifactMap{{Name: "note", Output: "note"}}, Failure: "leaf"}, "leaf": {Primitive: workflow.AgentTurn, Role: "author", Output: "Contract"}}
	d.Edges = []workflow.Edge{{From: "child", To: "Done"}, {From: "child", To: "leaf"}, {From: "leaf", To: "Done"}}
	return d
}
func TestRecursiveSelfChildRefusalRoutesVisitWithoutRejectingRoot(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.MaxInFlight = 2
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"leaf": {{Output: json.RawMessage(`{"text":"bounded recursion"}`)}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	doc := recursiveTreeWorkflow()
	doc.Artifacts["Contract"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "recursive-root", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "recursive-root"); err != nil {
		t.Fatal("root admission incorrectly rejected recursion", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		snap, e := d.store.GetJob(ctx, "recursive-root")
		if e == nil && snap.State == "Done" {
			break
		}
		if time.Now().After(deadline) {
			rows, _ := d.store.ListJobs(ctx, 100)
			for _, row := range rows {
				var state envelope
				_ = decodeEnvelope(row.Payload, &state)
				t.Logf("%s state=%s parked=%v reason=%s child=%+v history=%+v", row.ID, row.State, state.Job.Parked, state.Job.Reason, state.Job.Child, state.Job.History)
			}
			t.Fatalf("recursive tree did not finish: %v quarantined=%v", e, d.quarantined)
		}
		time.Sleep(20 * time.Millisecond)
	}
	grand, err := d.store.GetJob(ctx, "recursive-root.child.1.child.1")
	if err != nil {
		t.Fatal("depth3 grandchild absent", err)
	}
	var e envelope
	if decodeEnvelope(grand.Payload, &e) != nil || e.Job.TreeDepth != 3 {
		t.Fatal("wrong recursion metadata")
	}
	found := false
	for _, event := range e.Job.History {
		found = found || event.Kind == "sub-workflow-failed" && strings.Contains(event.Cause, "recursion_budget")
	}
	if !found {
		t.Fatal("depth refusal did not route failed visit", e.Job.History)
	}
	if _, found, err := d.storedJob(ctx, "recursive-root.child.1.child.1.child.1"); err != nil || found {
		t.Fatal("depth4 child admitted", err)
	}
}

func TestChildSnapshotForkUsesParentsRecordedVisitTree(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	parent, _, err := d.live.forkTask(ctx, "snapshot", "parent", cfg.Live.Base, "")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(parent.Path, "ontology.json")
	if err = os.WriteFile(file, []byte(`{"parent":"stage3"}`), 0600); err != nil {
		t.Fatal(err)
	}
	tree, err := d.live.broker.Tree(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	before := liveGit(t, parent.Path, "rev-parse", "HEAD")
	base, err := d.live.childVisitBase(ctx, engine.Job{TaskID: "parent", Tree: tree, Base: before})
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := d.live.forkTask(ctx, "snapshot", "child", base, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(child.Path, "ontology.json"))
	if err != nil || string(raw) != `{"parent":"stage3"}` {
		t.Fatal("child missed current parent file", string(raw), err)
	}
	if after := liveGit(t, parent.Path, "rev-parse", "HEAD"); after != before {
		t.Fatal("child snapshot moved parent HEAD")
	}
}

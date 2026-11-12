package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// childWorkflow drafts a note and waits for an operator decision.
func childWorkflow() workflow.Document {
	role := workflow.Role{Instructions: "instructions", Briefs: map[string]string{"default": "brief"}, Routes: []workflow.Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 2, Outputs: []string{"Note"}}
	return currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "child", Start: "draft", Limits: workflow.Limits{Attempts: 5, TestRuns: 1, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{"author": role},
		Artifacts: map[string]workflow.ArtifactType{"Note": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["text"]}`)}, "Decision": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}},
		Stages: map[string]workflow.Stage{
			"draft":   {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"},
			"approve": {Primitive: workflow.HumanDecision, Question: "Use this note?", Options: []workflow.DecisionOption{{ID: "use", Next: "Done"}, {ID: "halt", Next: "halt"}}, OnTimeout: workflow.StayWaiting, Output: "Decision"},
			"halt":    {Primitive: workflow.Escalate},
		},
		Edges: []workflow.Edge{{From: "draft", To: "approve"}, {From: "approve", To: "Done"}, {From: "approve", To: "halt"}},
	})
}

// parentWorkflow consults the pinned child between the test gate and review.
func parentWorkflow(revision int) workflow.Document {
	d := decisionWorkflow()
	d.ID = "parent"
	delete(d.Stages, "approve")
	delete(d.Stages, "again")
	delete(d.Artifacts, "Decision")
	d.Artifacts["Consult"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","required":["child_task","outputs"]}`)}
	d.Stages["consult"] = workflow.Stage{Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: "child", Revision: revision}, Output: "Consult", InputMap: []workflow.ArtifactMap{{Name: "change", Stage: "change"}}, OutputMap: []workflow.ArtifactMap{{Name: "note", Stage: "draft"}}}
	d.Edges = []workflow.Edge{{From: "change", To: "test"}, {From: "test", To: "gate"}, {From: "gate", To: "consult"}, {From: "gate", To: "stop"}, {From: "consult", To: "review"}, {From: "review", To: "commit"}, {From: "commit", To: "Done"}}
	return d
}

// FAE-WF-030 SubWorkflow end to end through the daemon: a parent admission
// pinning an unknown child revision is refused; a waiting parent's child is
// admitted as its own task on the pinned child revision with the mapped
// parent artifacts as content and the parent's file scope as its declared
// files; it survives a restart; its result binds back; cancelling a parent
// retires its unfinished child.
func TestSubWorkflowDaemonEndToEnd(t *testing.T) {
	cfg, ctx := fixture(t)
	script := decisionScript()
	script["draft"] = []engine.ScriptStep{{Output: json.RawMessage(`{"text":"consulted"}`)}}
	cfg.Executor = engine.NewScripted(script)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	for _, doc := range []workflow.Document{childWorkflow(), parentWorkflow(1)} {
		if _, e = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); e != nil {
			t.Fatal(e)
		}
	}
	unknown := parentWorkflow(7)
	unknown.ID = "dangling"
	if _, e = call(t, d, "WorkflowInstall", InstallParams{ID: unknown.ID, Document: unknown, AuthorizingMessage: "message"}, ""); e == nil || !strings.Contains(e.Error(), "footprint") {
		t.Fatal("unresolvable child footprint installed", e)
	}
	if _, _, e = d.store.GetWorkflow(ctx, unknown.ID, 0); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("refused workflow left a revision", e)
	}
	for _, id := range []string{"alpha", "beta"} {
		if _, e = call(t, d, "Admit", AdmitParams{ID: id, WorkflowID: "parent", Tree: "tree", AuthorizingMessage: "message", DeclaredFiles: []string{"pkg/a.go", "pkg/a_test.go"}}, id); e != nil {
			t.Fatal(e)
		}
	}
	awaitDecision(t, d, "alpha.consult.1", 1)
	awaitDecision(t, d, "beta.consult.1", 1)
	child, e := d.store.GetJob(ctx, "alpha.consult.1")
	var env envelope
	if e != nil || decodeEnvelope(child.Payload, &env) != nil || child.WorkflowID != "child" || child.WorkflowRevision != 1 || env.Job.Parent == nil || env.Job.Parent.Task != "alpha" || !strings.Contains(env.Admission.Content, `"parent_task":"alpha"`) {
		t.Fatal("child binding", child.WorkflowID, child.WorkflowRevision, e)
	}
	// The child works within its parent's resolved scope.
	if !slices.Equal(env.Admission.DeclaredFiles, []string{"pkg/a.go", "pkg/a_test.go"}) {
		t.Fatal("child declared files", env.Admission.DeclaredFiles)
	}
	stop(t, d)
	if d, e = Start(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "Decide", DecideParams{ID: "alpha.consult.1", Option: "use", AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	parent := awaitState(t, d, "alpha", "Done")
	if e = decodeEnvelope(parent.Payload, &env); e != nil {
		t.Fatal(e)
	}
	bound := false
	for _, a := range env.Job.Evidence {
		if a.Stage == "consult" && a.Session == "alpha.consult.1" && strings.Contains(mustJSON(a.Fields), `"text":"consulted"`) {
			bound = true
		}
	}
	if !bound {
		t.Fatal("child result not bound into the parent")
	}
	for _, method := range []string{"Block", "Cancel"} {
		if _, e = call(t, d, method, TaskControlParams{ID: "beta", Reason: "not needed", AuthorizingMessage: "message"}, ""); e != nil {
			t.Fatal(method, e)
		}
	}
	retired := awaitState(t, d, "beta.consult.1", "Retired")
	if e = decodeEnvelope(retired.Payload, &env); e != nil || !strings.HasPrefix(env.Job.Reason, "obsolete parent beta") {
		t.Fatal("cancelled parent's child", env.Job.Reason, e)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A SubWorkflow child's declared files are its parent's resolved scope: a
// derived parent's Spec files plus approved widens, a pinned parent's pinned
// files; a parent with no files yet gives none.
func TestSubWorkflowChildFilesAreParentScope(t *testing.T) {
	ctx := context.Background()
	d := &Daemon{live: &liveExecutor{}}
	e := scopeEnvelope(nil, specEvidence("pkg/a.go", "pkg/a_test.go"))
	e.Job.ApprovedScope = []string{"pkg/b.go"}
	files, err := d.subWorkflowChildFiles(ctx, "task", e)
	if err != nil || !slices.Equal(files, []string{"pkg/a.go", "pkg/a_test.go", "pkg/b.go"}) {
		t.Fatal("derived parent", files, err)
	}
	if files, err = d.subWorkflowChildFiles(ctx, "task", scopeEnvelope([]string{"z.go"}, specEvidence("pkg/a.go"))); err != nil || !slices.Equal(files, []string{"z.go"}) {
		t.Fatal("admission files", files, err)
	}
	if files, err = d.subWorkflowChildFiles(ctx, "task", scopeEnvelope(nil)); err != nil || files != nil {
		t.Fatal("parent without files", files, err)
	}
	if _, err = d.subWorkflowChildFiles(ctx, "task", scopeEnvelope(nil, specEvidence("sub/.git/config"))); !isScopeRefusal(err) {
		t.Fatal("unsafe parent scope", err)
	}
	d.live.config.Tasks = map[string]LiveTaskConfig{"task": {Files: []string{"p.go", "p_test.go"}, TestFile: "p_test.go", TestID: "TestP", ExpectedFailure: "p"}}
	if files, err = d.subWorkflowChildFiles(ctx, "task", e); err != nil || !slices.Equal(files, []string{"p.go", "p_test.go", "pkg/b.go"}) {
		t.Fatal("pinned parent", files, err)
	}
	d.live = nil
	if files, err = d.subWorkflowChildFiles(ctx, "task", scopeEnvelope([]string{"z.go"})); err != nil || !slices.Equal(files, []string{"z.go"}) {
		t.Fatal("scripted parent", files, err)
	}
}

func TestSubWorkflowMultiChildDaemonFanOutAndBind(t *testing.T) {
	cfg, ctx := fixture(t)
	script := decisionScript()
	script["change"] = []engine.ScriptStep{{Output: json.RawMessage(`{"summary":"changed","files":["pkg/a.go"],"items":[{"part":"one"},{"part":"two"},{"part":"three"}]}`)}}
	script["draft"] = []engine.ScriptStep{{Output: json.RawMessage(`{"text":"fanout-consulted"}`)}}
	cfg.Executor = engine.NewScripted(script)

	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)

	pDoc := parentWorkflow(1)
	consultStage := pDoc.Stages["consult"]
	consultStage.InputMap = []workflow.ArtifactMap{{Name: "items", Stage: "change"}}
	consultStage.ItemsFrom = &workflow.Projection{Stage: "change", Field: "items"}
	pDoc.Stages["consult"] = consultStage

	for _, doc := range []workflow.Document{childWorkflow(), pDoc} {
		if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
	}

	if _, err = call(t, d, "Admit", AdmitParams{ID: "fanparent", WorkflowID: "parent", Tree: "tree", AuthorizingMessage: "message", DeclaredFiles: []string{"pkg/a.go"}}, "fanparent"); err != nil {
		t.Fatal(err)
	}

	childIDs := []string{"fanparent.consult.1.0", "fanparent.consult.1.1", "fanparent.consult.1.2"}
	for _, cid := range childIDs {
		awaitDecision(t, d, cid, 1)
	}

	for i, cid := range childIDs {
		childSnap, err := d.store.GetJob(ctx, cid)
		var cenv envelope
		if err != nil || decodeEnvelope(childSnap.Payload, &cenv) != nil {
			t.Fatalf("load child %s: %v", cid, err)
		}
		if !strings.Contains(cenv.Admission.Content, `"item_index":`) {
			t.Fatalf("expected child %d (%s) admission content to include item_index, got %s", i, cid, cenv.Admission.Content)
		}
		if _, err = call(t, d, "Decide", DecideParams{ID: cid, Option: "use", AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatalf("Decide %s: %v", cid, err)
		}
	}

	parentSnap := awaitState(t, d, "fanparent", "Done")
	var penv envelope
	if err := decodeEnvelope(parentSnap.Payload, &penv); err != nil {
		t.Fatal(err)
	}
	foundChildren := false
	for _, a := range penv.Job.Evidence {
		if a.Stage == "consult" {
			if kids, ok := a.Fields["children"].([]any); ok && len(kids) == 3 {
				foundChildren = true
			}
		}
	}
	if !foundChildren {
		t.Fatalf("expected parent consult evidence to contain 3 bound children: %+v", penv.Job.Evidence)
	}
}

func TestSubWorkflowParentEscalateRestartBlockResumeAndOutputMapValidation(t *testing.T) {
	cfg, ctx := fixture(t)
	script := decisionScript()
	script["draft"] = []engine.ScriptStep{{Output: json.RawMessage(`{"text":"consulted"}`)}}
	cfg.Executor = engine.NewScripted(script)

	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)

	pDoc := parentWorkflow(1)
	pDoc.Stages["again"] = workflow.Stage{Primitive: workflow.Loop, MaxIterations: 2}
	cs := pDoc.Stages["consult"]
	cs.Failure = "again"
	pDoc.Stages["consult"] = cs
	pDoc.Edges = append(pDoc.Edges, workflow.Edge{From: "consult", To: "again"}, workflow.Edge{From: "again", To: "change"})

	for _, doc := range []workflow.Document{childWorkflow(), pDoc} {
		if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Verify output_map validation against child workflow at admission.
	badOutStage := parentWorkflow(1)
	badOutStage.ID = "bad-out-stage"
	bcs := badOutStage.Stages["consult"]
	bcs.OutputMap = []workflow.ArtifactMap{{Name: "note", Stage: "nonexistent"}}
	badOutStage.Stages["consult"] = bcs
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: badOutStage.ID, Document: badOutStage, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "bad1", WorkflowID: badOutStage.ID, Tree: "tree", AuthorizingMessage: "message"}, "bad1"); err == nil || !strings.Contains(err.Error(), "unknown stage nonexistent") {
		t.Fatalf("expected admission refusal for unknown child stage in output_map, got %v", err)
	}

	// Admit parent and wait for child visit 1.
	if _, err = call(t, d, "Admit", AdmitParams{ID: "cycle", WorkflowID: "parent", Tree: "tree", AuthorizingMessage: "message", DeclaredFiles: []string{"pkg/a.go"}}, "cycle"); err != nil {
		t.Fatal(err)
	}
	awaitDecision(t, d, "cycle.consult.1", 1)

	// Escalate -> RestartWithInformation while parked at SubWorkflowPending.
	if _, err = call(t, d, "Escalate", TaskControlParams{ID: "cycle", Reason: "pause", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "cycle.consult.1", "Retired")
	if _, err = call(t, d, "RestartWithInformation", TaskControlParams{ID: "cycle", Information: "retry visit 2", Reason: "restart", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitDecision(t, d, "cycle.consult.2", 1)

	// Block -> Resume while parked at SubWorkflowPending.
	if _, err = call(t, d, "Block", TaskControlParams{ID: "cycle", Reason: "block visit 2", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "cycle.consult.2", "Retired")
	if _, err = call(t, d, "Resume", TaskControlParams{ID: "cycle", Reason: "resume visit 3", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitDecision(t, d, "cycle.consult.3", 1)
	if _, err = call(t, d, "Decide", DecideParams{ID: "cycle.consult.3", Option: "use", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "cycle", "Done")
}

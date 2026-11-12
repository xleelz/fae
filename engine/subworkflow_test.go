package engine_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// childDoc drafts a note and gates on it.
func childDoc() workflow.Document {
	role := workflow.Role{Instructions: "instructions", Briefs: map[string]string{"default": "brief"}, Routes: []workflow.Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"Note"}}
	ok := bin("eq", fieldOf("draft", "ok"), lit(true))
	return workflow.Document{SchemaVersion: 1, ID: "child", Start: "draft", Limits: workflow.Limits{Attempts: 5, TestRuns: 1, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{"author": role},
		Artifacts: map[string]workflow.ArtifactType{"Note": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["ok"]}`)}},
		Stages: map[string]workflow.Stage{
			"draft": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"},
			"check": {Primitive: workflow.Gate, Expr: &ok, Failure: "halt"},
			"halt":  {Primitive: workflow.Escalate},
		},
		Edges: []workflow.Edge{{From: "draft", To: "check"}, {From: "check", To: "Done"}, {From: "check", To: "halt"}},
	}
}

// subDoc consults the child workflow between the test gate and the review.
func subDoc(failure bool) workflow.Document {
	d := floorDoc()
	d.Artifacts["Consult"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","required":["child_task","outputs"],"properties":{"outputs":{"type":"object","required":["note"]}}}`)}
	s := workflow.Stage{Primitive: workflow.SubWorkflow, Workflow: &workflow.WorkflowRef{ID: "child", Revision: 1}, Output: "Consult", InputMap: []workflow.ArtifactMap{{Name: "change", Stage: "change"}}, OutputMap: []workflow.ArtifactMap{{Name: "note", Stage: "draft", Type: "Note"}}}
	if failure {
		s.Failure = "stop"
		d.Edges = append(d.Edges, workflow.Edge{From: "consult", To: "stop"})
	}
	d.Stages["consult"] = s
	r := d.Stages["review"]
	r.Inputs = []workflow.Input{{Name: "consult", Type: "Consult", Stage: "consult", Version: 1}}
	d.Stages["review"] = r
	replaceEdge(&d, workflow.Edge{From: "gate", To: "review"}, workflow.Edge{From: "gate", To: "consult"}, workflow.Edge{From: "consult", To: "review"})
	return d
}
func childJob(t *testing.T, ref engine.ChildRef, parent string, ok bool) engine.Job {
	t.Helper()
	c, e := engine.New(ref.Task, ref.Revision, childDoc(), "tree")
	if e != nil {
		t.Fatal(e)
	}
	c.Parent = &engine.ParentRef{Task: parent, Stage: ref.Stage, Visit: ref.Visit}
	output := `{"ok":true,"text":"consulted"}`
	if !ok {
		output = `{"ok":false}`
	}
	return drive(t, c, engine.NewScripted(map[string][]engine.ScriptStep{"draft": step(output)}), true)
}

// FAE-WF-030 SubWorkflow conformance: the parent waits for a pinned child task
// with its own workflow revision, the child's mapped artifacts are bound back
// with provenance, and only a matching finished child is ever bound.
func TestSubWorkflowBindsPinnedChild(t *testing.T) {
	x := engine.NewScripted(floorScript(1))
	parent := drive(t, newPrimitiveJob(t, subDoc(false)), x, true)
	ref, ok := engine.PendingChild(roundTrip(t, parent))
	if !ok || ref.Stage != "consult" || ref.Visit != 1 || ref.Task != "task.consult.1" || ref.WorkflowID != "child" || ref.Revision != 1 || parent.Reason != engine.ParkSubWorkflow {
		t.Fatal(ref, ok, parent.Reason)
	}
	if next, d, events, e := engine.Prepare(parent); e != nil || d != nil || len(events) != 0 || !next.Parked {
		t.Fatal("a waiting parent moved")
	}
	content, e := engine.ChildInputs(parent)
	if e != nil || !strings.Contains(string(content), `"parent_task":"task"`) || !strings.Contains(string(content), `"change":{"artifact_id"`) {
		t.Fatal(string(content), e)
	}
	tampered := roundTrip(t, parent)
	tampered.Child.Revision = 2
	if engine.Validate(tampered) == nil {
		t.Fatal("re-pinned child reference validated")
	}
	if _, _, e = engine.Migrate(parent, subDoc(true), 2, ""); e == nil {
		t.Fatal("migration while waiting for a sub-workflow")
	}
	child := childJob(t, ref, "task", true)
	if child.State != domain.Done {
		t.Fatal(child.State)
	}
	running, _ := engine.New(ref.Task, 1, childDoc(), "tree")
	running.Parent = child.Parent
	wrongRevision := child
	wrongRevision.Revision = 2
	unlinked := child
	unlinked.Parent = &engine.ParentRef{Task: "task", Stage: "consult", Visit: 2}
	for name, c := range map[string]engine.Job{"running": running, "revision": wrongRevision, "link": unlinked} {
		if _, _, e := engine.BindChild(parent, c); e == nil {
			t.Fatal(name, "child bound")
		}
	}
	bound, events, e := engine.BindChild(parent, child)
	if e != nil || bound.State != "review" || bound.Child != nil || bound.Parked || events[len(events)-1].To != "review" {
		t.Fatal(bound.State, e)
	}
	if _, _, e = engine.BindChild(bound, child); e == nil {
		t.Fatal("child bound twice")
	}
	var consult workflow.Evidence
	for _, a := range bound.Evidence {
		if a.Stage == "consult" {
			consult = a
		}
	}
	outputs, _ := consult.Fields["outputs"].(map[string]any)
	note, _ := outputs["note"].(map[string]any)
	if consult.Role != "sub-workflow" || consult.Session != ref.Task || note["text"] != "consulted" || consult.Fields["workflow_hash"] != child.WorkflowHash {
		t.Fatal(consult)
	}
	for _, p := range bound.Provenance {
		if p.ArtifactID == consult.ID && (p.Primitive != workflow.SubWorkflow || len(p.Inputs) != 1 || p.Inputs[0] != child.Evidence[0].ID) {
			t.Fatal("binding provenance", p)
		}
	}
	if done := drive(t, bound, x, true); done.State != domain.Done {
		t.Fatal(done.State, done.Reason)
	}
}

// A child that ends without Done fails the visit: the failure route when the
// stage declares one, else an escalation; an obsolete child is detected for
// cancellation once its parent no longer waits for it.
func TestSubWorkflowFailureAndCancellation(t *testing.T) {
	x := engine.NewScripted(floorScript(2))
	for _, failure := range []bool{true, false} {
		parent := drive(t, newPrimitiveJob(t, subDoc(failure)), x, false)
		ref, _ := engine.PendingChild(parent)
		child := childJob(t, ref, "task", false)
		retired, _, e := engine.ApplyControl(child, engine.Control{Method: "Retire", Reason: "obsolete ruled out"}, time.Now())
		if e != nil || child.State != domain.Escalated {
			t.Fatal(child.State, e)
		}
		failed, _, e := engine.BindChild(parent, retired)
		if e != nil {
			t.Fatal(e)
		}
		if failure && failed.State != "stop" || !failure && (failed.State != domain.Escalated || !strings.HasPrefix(failed.EscalationDetail, "SubWorkflowFailed: child task.consult.1 ended Retired")) {
			t.Fatal(failure, failed.State, failed.EscalationDetail)
		}
	}
	d := subDoc(true)
	c := d.Stages["consult"]
	c.OutputMap = []workflow.ArtifactMap{{Name: "note", Stage: "missing"}}
	d.Stages["consult"] = c
	parent := drive(t, newPrimitiveJob(t, d), x, false)
	ref, _ := engine.PendingChild(parent)
	if failed, _, e := engine.BindChild(parent, childJob(t, ref, "task", true)); e != nil || failed.State != "stop" {
		t.Fatal("a missing mapped output must fail the visit", failed.State, e)
	}
	child := childJob(t, ref, "task", true)
	child.State = "draft"
	if engine.Obsolete(child, &parent) {
		t.Fatal("waited-for child reported obsolete")
	}
	now := time.Now().UTC()
	blocked, _, e := engine.ApplyControl(parent, engine.Control{Method: "Block", Reason: "hold"}, now)
	if e != nil {
		t.Fatal(e)
	}
	cancelled, _, e := engine.ApplyControl(blocked, engine.Control{Method: "Cancel", Reason: "not needed"}, now)
	if e != nil || !engine.Obsolete(child, &cancelled) || !engine.Obsolete(child, nil) {
		t.Fatal("a cancelled parent's child must be obsolete", e)
	}
	if _, _, e = engine.BindChild(cancelled, childJob(t, ref, "task", true)); e == nil {
		t.Fatal("bound into a cancelled parent")
	}
}

func fanOutSubDoc() workflow.Document {
	d := subDoc(true)
	c := d.Stages["consult"]
	c.InputMap = []workflow.ArtifactMap{{Name: "items", Stage: "change"}}
	c.ItemsFrom = &workflow.Projection{Stage: "change", Field: "items"}
	d.Stages["consult"] = c
	return d
}

// TestSubWorkflowMultiChildFanOutAndBindChildren verifies 1-to-N SubWorkflow
// fan-out when the mapped parent artifact contains an "items" array of 3
// objects, per-child ChildInputItems generation, Obsolete tracking for all N
// children, and BindChildren aggregation into parent Evidence.
func TestSubWorkflowMultiChildFanOutAndBindChildren(t *testing.T) {
	script := floorScript(1)
	script["change"] = step(`{"summary":"changed","files":["a.go"],"items":[{"module":"auth"},{"module":"db"},{"module":"api"}]}`)
	x := engine.NewScripted(script)

	parent := drive(t, newPrimitiveJob(t, fanOutSubDoc()), x, true)
	ref, ok := engine.PendingChild(roundTrip(t, parent))
	if !ok || ref.Stage != "consult" || ref.Visit != 1 || ref.Task != "task.consult.1.0" {
		t.Fatalf("unexpected primary PendingChild: ref=%+v ok=%v", ref, ok)
	}
	childrenRefs := parent.Children()
	if len(childrenRefs) != 3 {
		t.Fatalf("expected 3 children refs, got %d: %+v", len(childrenRefs), childrenRefs)
	}
	for i := 0; i < 3; i++ {
		wantID := engine.ChildIDIndexed("task", "consult", 1, i)
		if childrenRefs[i].Task != wantID || childrenRefs[i].WorkflowID != "child" || childrenRefs[i].Revision != 1 {
			t.Fatalf("child %d mismatch: got %+v, want Task=%q", i, childrenRefs[i], wantID)
		}
	}

	items, err := engine.ChildInputItems(&parent)
	if err != nil || len(items) != 3 {
		t.Fatalf("ChildInputItems got len=%d err=%v", len(items), err)
	}
	wantModules := []string{"auth", "db", "api"}
	for i, raw := range items {
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		itemObj, _ := payload["item"].(map[string]any)
		idxFloat, _ := payload["item_index"].(float64)
		if itemObj["module"] != wantModules[i] || int(idxFloat) != i {
			t.Fatalf("item %d payload mismatch: %s", i, string(raw))
		}
	}

	children := make([]*engine.Job, 3)
	for i, cref := range childrenRefs {
		c, err := engine.New(cref.Task, cref.Revision, childDoc(), "tree")
		if err != nil {
			t.Fatal(err)
		}
		c.Parent = &engine.ParentRef{Task: "task", Stage: cref.Stage, Visit: cref.Visit}
		if engine.Obsolete(c, &parent) {
			t.Fatalf("active child %d (%s) should not be obsolete while parent waits", i, cref.Task)
		}
		doneChild := drive(t, c, engine.NewScripted(map[string][]engine.ScriptStep{
			"draft": step(`{"ok":true,"text":"note-` + wantModules[i] + `"}`),
		}), true)
		if doneChild.State != domain.Done {
			t.Fatalf("child %d state = %s, want Done", i, doneChild.State)
		}
		children[i] = &doneChild
	}

	if err := engine.BindChildren(&parent, children); err != nil {
		t.Fatalf("BindChildren failed: %v", err)
	}
	if parent.State != "review" || parent.Parked || parent.Child != nil || len(parent.Children()) != 0 {
		t.Fatalf("unexpected parent state after BindChildren: state=%s parked=%v child=%+v", parent.State, parent.Parked, parent.Child)
	}

	var consult workflow.Evidence
	for _, a := range parent.Evidence {
		if a.Stage == "consult" {
			consult = a
		}
	}
	boundChildren, ok := consult.Fields["children"].([]any)
	if !ok || len(boundChildren) != 3 {
		t.Fatalf("expected 3 bound children in consult evidence, got: %#v", consult.Fields)
	}
	for i, bc := range boundChildren {
		m, _ := bc.(map[string]any)
		note, _ := m["note"].(map[string]any)
		if note["text"] != "note-"+wantModules[i] {
			t.Fatalf("bound child %d note = %#v, want note-%s", i, note, wantModules[i])
		}
	}

	if done := drive(t, parent, x, true); done.State != domain.Done {
		t.Fatalf("expected parent to reach Done, got %s (%s)", done.State, done.Reason)
	}
}

func TestSubWorkflowEscalateRestartAndBlockResume(t *testing.T) {
	now := time.Now().UTC()
	x := engine.NewScripted(floorScript(4))

	// 1. Escalate -> Migrate / RestartWithInformation from SubWorkflowPending.
	parent := drive(t, newPrimitiveJob(t, subDoc(false)), x, false)
	ref1, ok := engine.PendingChild(parent)
	if !ok || ref1.Visit != 1 || ref1.Task != "task.consult.1" {
		t.Fatalf("expected parked at visit 1, got ref=%+v ok=%v", ref1, ok)
	}
	child1, err := engine.New(ref1.Task, ref1.Revision, childDoc(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	child1.Parent = &engine.ParentRef{Task: "task", Stage: ref1.Stage, Visit: ref1.Visit}

	escalated, _, err := engine.ApplyControl(parent, engine.Control{Method: "Escalate", Reason: "pause sub-workflow"}, now)
	if err != nil {
		t.Fatalf("Escalate failed: %v", err)
	}
	if escalated.Child != nil || !engine.Obsolete(child1, &escalated) {
		t.Fatalf("expected Child=nil and child1 obsolete after Escalate, got Child=%+v", escalated.Child)
	}
	if _, _, err := engine.Migrate(escalated, subDoc(true), 2, ""); err != nil {
		t.Fatalf("Migrate after Escalate failed: %v", err)
	}
	restarted, _, err := engine.ApplyControl(escalated, engine.Control{Method: "RestartWithInformation", Information: "retry consult", Reason: "retry"}, now)
	if err != nil {
		t.Fatalf("RestartWithInformation failed: %v", err)
	}
	if restarted.Child != nil || restarted.State != "change" {
		t.Fatalf("unexpected restarted state=%s child=%+v", restarted.State, restarted.Child)
	}
	parent2 := drive(t, restarted, x, false)
	ref2, ok := engine.PendingChild(parent2)
	if !ok || ref2.Visit != 2 || ref2.Task != "task.consult.2" || !engine.Obsolete(child1, &parent2) {
		t.Fatalf("expected visit 2 after restart, got ref=%+v ok=%v", ref2, ok)
	}

	// 2. Block -> Resume from SubWorkflowPending (with remediation route via Loop to change).
	remedDoc := subDoc(false)
	remedDoc.Stages["again"] = workflow.Stage{Primitive: workflow.Loop, MaxIterations: 2}
	cs := remedDoc.Stages["consult"]
	cs.Failure = "again"
	remedDoc.Stages["consult"] = cs
	remedDoc.Edges = append(remedDoc.Edges, workflow.Edge{From: "consult", To: "again"}, workflow.Edge{From: "again", To: "change"})

	bParent := drive(t, newPrimitiveJob(t, remedDoc), x, false)
	bRef1, ok := engine.PendingChild(bParent)
	if !ok || bRef1.Visit != 1 {
		t.Fatalf("expected bParent parked at visit 1, got %+v", bRef1)
	}
	bChild1, err := engine.New(bRef1.Task, bRef1.Revision, childDoc(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	bChild1.Parent = &engine.ParentRef{Task: "task", Stage: bRef1.Stage, Visit: bRef1.Visit}

	blocked, _, err := engine.ApplyControl(bParent, engine.Control{Method: "Block", Reason: "hold consult"}, now)
	if err != nil {
		t.Fatalf("Block failed: %v", err)
	}
	if blocked.Child != nil || !engine.Obsolete(bChild1, &blocked) {
		t.Fatalf("expected Child=nil and bChild1 obsolete after Block, got Child=%+v", blocked.Child)
	}
	resumed, _, err := engine.ApplyControl(blocked, engine.Control{Method: "Resume", Reason: "unblock consult"}, now)
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	if resumed.Child != nil || resumed.State != "again" {
		t.Fatalf("unexpected resumed state=%s child=%+v", resumed.State, resumed.Child)
	}
	bParent2 := drive(t, resumed, x, false)
	bRef2, ok := engine.PendingChild(bParent2)
	if !ok || bRef2.Visit != 2 || bRef2.Task != "task.consult.2" || !engine.Obsolete(bChild1, &bParent2) {
		t.Fatalf("expected visit 2 after Block->Resume, got ref=%+v ok=%v", bRef2, ok)
	}
}

func TestSubWorkflowFanOutOverMaxItemsAndBindChildrenFailures(t *testing.T) {
	// 1. > 16 items fails the visit immediately via ChildFailed instead of silent 1-to-1 fallback.
	var seventeen []string
	for i := 0; i < 17; i++ {
		seventeen = append(seventeen, `{"idx":1}`)
	}
	script17 := floorScript(1)
	script17["change"] = step(`{"summary":"changed","files":["a.go"],"items":[` + strings.Join(seventeen, ",") + `]}`)
	docNoFail := subDoc(false)
	cs := docNoFail.Stages["consult"]
	cs.InputMap = []workflow.ArtifactMap{{Name: "items", Stage: "change"}}
	cs.ItemsFrom = &workflow.Projection{Stage: "change", Field: "items"}
	docNoFail.Stages["consult"] = cs
	parent17 := drive(t, newPrimitiveJob(t, docNoFail), engine.NewScripted(script17), false)
	if parent17.State != domain.Escalated || parent17.Child != nil || parent17.Parked || !strings.Contains(parent17.EscalationDetail, "sub-workflow fan-out items count 17 exceeds maximum 16") {
		t.Fatalf("expected >16 items to fail via ChildFailed, got state=%s detail=%q parked=%v child=%+v", parent17.State, parent17.EscalationDetail, parent17.Parked, parent17.Child)
	}

	// 2. BindChildren transitions parent via ChildFailed and returns nil when a child is not Done or lacks output.
	script2 := floorScript(2)
	script2["change"] = step(`{"summary":"changed","files":["a.go"],"items":[{"module":"a"},{"module":"b"}]}`)
	x := engine.NewScripted(script2)
	parent := drive(t, newPrimitiveJob(t, fanOutSubDoc()), x, false)
	refs := parent.Children()
	if len(refs) != 2 {
		t.Fatalf("expected 2 children, got %d", len(refs))
	}
	c0 := childJob(t, refs[0], "task", true)
	c1Escalated := childJob(t, refs[1], "task", false)
	c1Retired, _, err := engine.ApplyControl(c1Escalated, engine.Control{Method: "Retire", Reason: "obsolete bad item"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.BindChildren(&parent, []*engine.Job{&c0, &c1Retired}); err != nil {
		t.Fatalf("expected BindChildren to handle non-Done child via ChildFailed and return nil, got err=%v", err)
	}
	if parent.State != "stop" || parent.Child != nil || parent.Parked {
		t.Fatalf("expected parent routed to 'stop' after non-Done child, got state=%s parked=%v child=%+v", parent.State, parent.Parked, parent.Child)
	}
}

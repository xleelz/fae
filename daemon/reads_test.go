package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func TestBoardBoundedSortedCachedPagination(t *testing.T) {
	d := &Daemon{jobs: map[string]controlplane.JobSnapshot{}, reads: make(chan struct{}, 1)}
	for i := 802; i >= 0; i-- {
		id := fmt.Sprintf("task-%04d", i)
		payload, _ := json.Marshal(envelope{Job: engine.Job{Reason: "waiting"}, Admission: AdmitParams{Priority: 10, Mode: "Local", RunID: "run", Content: strings.Repeat("not in board", 2000)}})
		d.jobs[id] = controlplane.JobSnapshot{ID: id, Version: 7, WorkflowID: "workflow", WorkflowRevision: 2, TargetID: "/workspace", TargetRevision: 1, State: "Pending", Payload: payload}
	}
	d.publishStatus()
	// A saturated SQL read pool and the writer lock cannot block cached Board.
	d.reads <- struct{}{}
	d.mu.Lock()
	defer d.mu.Unlock()
	after := ""
	seen := map[string]bool{}
	last := ""
	for {
		raw, _ := json.Marshal(BoardParams{After: after, Limit: 73})
		result, err := d.handle(context.Background(), api.Request{Method: "Board", Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		page := result.(BoardPage)
		if len(page.Items) > 73 || !fits(page) {
			t.Fatal("page bound exceeded")
		}
		encoded, _ := json.Marshal(page)
		if strings.Contains(string(encoded), "not in board") || strings.Contains(string(encoded), "payload") {
			t.Fatal("board leaked full payload")
		}
		for _, item := range page.Items {
			if item.ID <= last || seen[item.ID] {
				t.Fatal("duplicate or unordered task", item.ID)
			}
			seen[item.ID] = true
			last = item.ID
			if item.Priority != 10 || item.WorkflowRevision != 2 {
				t.Fatal("lost metadata")
			}
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor != last {
			t.Fatal("cursor differs from last item")
		}
		after = page.NextCursor
	}
	if len(seen) != 803 {
		t.Fatalf("lost tasks: %d", len(seen))
	}
	for _, limit := range []int{-1, 501} {
		raw, _ := json.Marshal(BoardParams{Limit: limit})
		if _, err := d.board(context.Background(), raw); err == nil {
			t.Fatal("bad limit accepted")
		}
	}
	// Published snapshots are immutable until the next writer publication.
	delete(d.jobs, "task-0000")
	page, err := d.board(context.Background(), nil)
	if err != nil || page.Items[0].ID != "task-0000" || len(page.Items) != 100 {
		t.Fatal("cache mutated or default bound incorrect")
	}
}
func TestBoardShrinksToEncodedBudget(t *testing.T) {
	d := &Daemon{}
	s := statusSnapshot{Updated: time.Now()}
	for i := 0; i < 20; i++ {
		s.Tasks = append(s.Tasks, BoardItem{JobIdentity: JobIdentity{ID: fmt.Sprintf("%02d", i)}, Reason: strings.Repeat("x", 100000)})
	}
	d.status.Store(s)
	page, err := d.board(context.Background(), json.RawMessage(`{"limit":20}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) >= 20 || len(page.Items) == 0 || page.NextCursor == "" || !fits(page) {
		t.Fatal("encoded bound not enforced")
	}
}
func TestHistoryPageReferencesFollowCompletionCursor(t *testing.T) {
	events := []controlplane.Event{}
	refs := []controlplane.ProvenanceRecord{}
	for i := 1; i <= 11; i++ {
		token := fmt.Sprintf("dispatch-%d", i)
		payload, _ := json.Marshal(engine.Event{Kind: "transition", Cause: "completed " + token})
		events = append(events, controlplane.Event{Sequence: int64(i), Payload: payload})
		refs = append(refs, controlplane.ProvenanceRecord{ArtifactID: fmt.Sprintf("artifact-%d", i), DispatchID: token, Stage: "Build", Tree: "tree"})
	}
	snapshot := controlplane.JobSnapshot{ID: "task", Payload: json.RawMessage(`{"document":"must not return"}`)}
	first, err := historyPage(snapshot, refs, events[:6], 5)
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor != 5 || len(first.Events) != 5 || len(first.ArtifactReferences) != 5 {
		t.Fatal("incorrect first page")
	}
	second, err := historyPage(snapshot, refs, events[5:], 6)
	if err != nil {
		t.Fatal(err)
	}
	if second.NextCursor != 0 || len(second.ArtifactReferences) != 6 {
		t.Fatal("incorrect final page")
	}
	seen := map[string]bool{}
	for _, page := range []HistoryPage{first, second} {
		encoded, _ := json.Marshal(page)
		if strings.Contains(string(encoded), "must not return") {
			t.Fatal("returned workflow body")
		}
		for _, r := range page.ArtifactReferences {
			if seen[r.ArtifactID] {
				t.Fatal("duplicate reference")
			}
			seen[r.ArtifactID] = true
		}
	}
	if len(seen) != 11 {
		t.Fatal("missing reference")
	}
}
func TestHistoryShrinksOrRefusesOversizedEvent(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"data": strings.Repeat("x", 100000)})
	events := []controlplane.Event{}
	for i := 0; i < 20; i++ {
		events = append(events, controlplane.Event{Sequence: int64(i + 1), Payload: payload})
	}
	page, err := historyPage(controlplane.JobSnapshot{ID: "task"}, nil, events, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) >= 20 || page.NextCursor == 0 || !fits(page) {
		t.Fatal("history size bound missing")
	}
	huge, _ := json.Marshal(strings.Repeat("x", readPageBytes))
	_, err = historyPage(controlplane.JobSnapshot{ID: "task"}, nil, []controlplane.Event{{Payload: huge}}, 1)
	var typed *api.RPCError
	if !errors.As(err, &typed) || typed.Code != "response-too-large" {
		t.Fatalf("missing typed bound: %v", err)
	}
}

func TestLegacyHistoryRefusesOversizedPayload(t *testing.T) {
	small := controlplane.JobSnapshot{ID: "small", Payload: json.RawMessage(`{"job":{}}`)}
	result, err := legacyHistory(small)
	if err != nil || result.(controlplane.JobSnapshot).ID != "small" {
		t.Fatal("small legacy response changed")
	}
	large, _ := json.Marshal(strings.Repeat("x", readPageBytes))
	_, err = legacyHistory(controlplane.JobSnapshot{ID: "large", Payload: large})
	var typed *api.RPCError
	if !errors.As(err, &typed) || typed.Code != "response-too-large" || !strings.Contains(typed.Message, "summary=true") {
		t.Fatalf("missing actionable typed refusal: %v", err)
	}
}

func TestSubWorkflowReadsExposeHierarchyAndStageWorkflow(t *testing.T) {
	// Verify graphOf and placeTopologyTasks populate TopologyStage.Workflow on SubWorkflow stages.
	g := graphOf(parentWorkflow(2))
	placed := placeTopologyTasks(controlplane.Molecule{ID: "mol-parent", WorkflowID: "parent", Revision: 1}, g, []controlplane.TopologyTask{{ID: "alpha", State: "consult"}})
	var consult *TopologyStage
	for i := range placed.Stages {
		if placed.Stages[i].Stage == "consult" {
			consult = &placed.Stages[i]
		} else if placed.Stages[i].Workflow != nil {
			t.Fatalf("non-SubWorkflow stage %s has Workflow %+v", placed.Stages[i].Stage, placed.Stages[i].Workflow)
		}
	}
	if consult == nil || consult.Primitive != workflow.SubWorkflow || consult.Workflow == nil || *consult.Workflow != (workflow.WorkflowRef{ID: "child", Revision: 2}) {
		t.Fatalf("consult stage = %+v, want SubWorkflow -> child@r2", consult)
	}

	// Verify TaskGet, Board, and Topology end-to-end on a live SubWorkflow parent/child pair.
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
	for _, doc := range []workflow.Document{childWorkflow(), parentWorkflow(1)} {
		if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "alpha", WorkflowID: "parent", Tree: "tree", AuthorizingMessage: "message", DeclaredFiles: []string{"pkg/a.go", "pkg/a_test.go"}}, "alpha"); err != nil {
		t.Fatal(err)
	}
	awaitDecision(t, d, "alpha.consult.1", 1)

	wantChild := engine.ChildRef{Stage: "consult", Visit: 1, Task: "alpha.consult.1", WorkflowID: "child", Revision: 1}
	wantParent := engine.ParentRef{Task: "alpha", Stage: "consult", Visit: 1}

	var parentView, childView TaskView
	if err := taskCall(t, d, "TaskGet", TaskGetParams{ID: "alpha"}, &parentView); err != nil {
		t.Fatal(err)
	}
	if parentView.Child == nil || !reflect.DeepEqual(*parentView.Child, wantChild) || parentView.Parent != nil {
		t.Fatalf("parent TaskGet hierarchy: parent=%+v child=%+v, want nil and %+v", parentView.Parent, parentView.Child, wantChild)
	}
	if err := taskCall(t, d, "TaskGet", TaskGetParams{ID: "alpha.consult.1"}, &childView); err != nil {
		t.Fatal(err)
	}
	if childView.Parent == nil || !reflect.DeepEqual(*childView.Parent, wantParent) || childView.Child != nil {
		t.Fatalf("child TaskGet hierarchy: parent=%+v child=%+v, want %+v and nil", childView.Parent, childView.Child, wantParent)
	}

	rows := boardRows(t, d, false)
	if pRow, ok := rows["alpha"]; !ok || pRow.Child == nil || !reflect.DeepEqual(*pRow.Child, wantChild) || pRow.Parent != nil {
		t.Fatalf("parent BoardItem = %+v, want Child=%+v Parent=nil", pRow, wantChild)
	}
	if cRow, ok := rows["alpha.consult.1"]; !ok || cRow.Parent == nil || !reflect.DeepEqual(*cRow.Parent, wantParent) || cRow.Child != nil {
		t.Fatalf("child BoardItem = %+v, want Parent=%+v Child=nil", cRow, wantParent)
	}

	var topo TopologyPage
	if err := taskCall(t, d, "Topology", TopologyParams{WorkflowID: "parent"}, &topo); err != nil {
		t.Fatal(err)
	}
	if len(topo.Molecules) != 1 {
		t.Fatalf("Topology molecules for parent workflow = %d, want 1", len(topo.Molecules))
	}
	foundStage := false
	for _, st := range topo.Molecules[0].Stages {
		if st.Stage == "consult" {
			foundStage = true
			if st.Workflow == nil || *st.Workflow != (workflow.WorkflowRef{ID: "child", Revision: 1}) {
				t.Fatalf("Topology consult stage Workflow = %+v, want child@r1", st.Workflow)
			}
		}
	}
	if !foundStage {
		t.Fatalf("Topology molecule stages %+v missing consult stage", topo.Molecules[0].Stages)
	}

	// Verify boardPage deep-copies Child.Children so mutating a BoardPage item cannot mutate the snapshot.
	s, _ := d.status.Load().(statusSnapshot)
	for i := range s.Tasks {
		if s.Tasks[i].ID == "alpha" && s.Tasks[i].Child != nil {
			s.Tasks[i].Child.Children = []engine.ChildRef{{Stage: "consult", Visit: 1, Task: "alpha.consult.1.0", WorkflowID: "child", Revision: 1}}
		}
	}
	d.status.Store(s)
	page, err := d.board(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range page.Items {
		if page.Items[i].ID == "alpha" && page.Items[i].Child != nil && len(page.Items[i].Child.Children) == 1 {
			page.Items[i].Child.Children[0].Task = "mutated"
		}
	}
	sAfter, _ := d.status.Load().(statusSnapshot)
	for i := range sAfter.Tasks {
		if sAfter.Tasks[i].ID == "alpha" && sAfter.Tasks[i].Child != nil && len(sAfter.Tasks[i].Child.Children) == 1 {
			if sAfter.Tasks[i].Child.Children[0].Task != "alpha.consult.1.0" {
				t.Fatalf("BoardPage mutated shared Child.Children slice: %+v", sAfter.Tasks[i].Child.Children)
			}
		}
	}

	// Finish child and parent, restart daemon, and verify Board preserves Parent on the finished child task.
	if _, err = call(t, d, "Decide", DecideParams{ID: "alpha.consult.1", Option: "use", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "alpha", "Done")
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	rowsAfterRestart := boardRows(t, d, false)
	if cRow, ok := rowsAfterRestart["alpha.consult.1"]; !ok || cRow.Parent == nil || !reflect.DeepEqual(*cRow.Parent, wantParent) {
		t.Fatalf("after restart, finished child BoardItem = %+v, want Parent=%+v", cRow, wantParent)
	}
}

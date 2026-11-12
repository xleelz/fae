package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

// twoStageWorkflow is shortWorkflow with a second agent stage, so a task's
// next stage dispatches a second turn.
func twoStageWorkflow() workflow.Document {
	doc := shortWorkflow()
	doc.ID = "two-stage"
	doc.Stages["review"] = workflow.Stage{Primitive: workflow.AgentTurn, Role: "worker", Output: "Report"}
	doc.Edges = []workflow.Edge{{From: "work", To: "review"}, {From: "review", To: "check"}, {From: "check", To: "Done"}, {From: "check", To: "stop"}}
	return doc
}

func installWorkflow(t *testing.T, d *Daemon, doc workflow.Document) {
	t.Helper()
	if _, err := call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
}

func newTask(workflowID, id string, edges ...controlplane.BacklogEdge) TaskCreateParams {
	return TaskCreateParams{BacklogItemParams: BacklogItemParams{AdmitParams: AdmitParams{ID: id, WorkflowID: workflowID, Tree: "tree", Content: "do " + id, AuthorizingMessage: "message"}}, Edges: edges}
}

func blocking(from, to string) controlplane.BacklogEdge {
	return controlplane.BacklogEdge{From: from, To: to, Kind: "blocking"}
}

// taskCall makes one live task RPC and decodes its result into out.
func taskCall(t *testing.T, d *Daemon, method string, p any, out any) error {
	t.Helper()
	r, err := call(t, d, method, p, "")
	if err == nil && out != nil {
		if err = json.Unmarshal(r.Result, out); err != nil {
			t.Fatal(method, err)
		}
	}
	return err
}

func refusedWith(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want refusal containing %q, got %v", want, err)
	}
}

func startedDispatch(t *testing.T, ctx context.Context, started <-chan engine.Dispatch) engine.Dispatch {
	t.Helper()
	select {
	case dispatch := <-started:
		return dispatch
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(3 * time.Minute):
		t.Fatal("no dispatch started")
	}
	return engine.Dispatch{}
}

func taskView(t *testing.T, d *Daemon, id string, includeRetired bool) TaskView {
	t.Helper()
	var v TaskView
	if err := taskCall(t, d, "TaskGet", TaskGetParams{ID: id, IncludeRetired: includeRetired}, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func stopTheDrive(t *testing.T, d *Daemon) {
	t.Helper()
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
}

func auditCount(t *testing.T, ctx context.Context, d *Daemon, action, actor string) int {
	t.Helper()
	n := 0
	err := d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "count audits", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE action=? AND actor=?`, action, actor).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TaskCreate (Dolt): one task joins the running graph through backlog
// admission's validation and is dispatched once its prerequisite is Done;
// an existing identity, a cycle (named), an unknown endpoint, a low
// priority and an edge not naming the new task are refused with nothing
// written; a new hold on a dispatched dependent needs the operator's
// acceptance. An edge added to and removed from a running task is in its
// TaskGet edges and edits and in BacklogShow.
func TestTaskCreateJoinsTheLiveGraphAndIsDispatched(t *testing.T) {
	cfg, ctx := fixture(t)
	ok := engine.ScriptStep{Output: json.RawMessage(`{"ok":true}`)}
	x := &controlGateExecutor{started: make(chan engine.Dispatch, 100), release: make(chan struct{}), delegate: engine.NewScripted(map[string][]engine.ScriptStep{"work": {ok, ok, ok, ok, ok}})}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := shortWorkflow()
	installWorkflow(t, d, doc)
	if err = taskCall(t, d, "TaskCreate", newTask(doc.ID, "a"), nil); err != nil {
		t.Fatal(err)
	}
	if first := startedDispatch(t, ctx, x.started); first.TaskID != "a" {
		t.Fatal("first dispatch", first.TaskID)
	}
	var created struct {
		ID         string                     `json:"id"`
		State      string                     `json:"state"`
		EdgesAdded []controlplane.BacklogEdge `json:"edges_added"`
		Readiness  map[string]any             `json:"readiness"`
	}
	if err = taskCall(t, d, "TaskCreate", newTask(doc.ID, "n", blocking("a", "n")), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "n" || created.State != "Pending" || len(created.EdgesAdded) != 1 || created.Readiness["ready"] != false || !reflect.DeepEqual(created.Readiness["withheld_by"], []any{"a: work"}) {
		t.Fatalf("created %+v", created)
	}
	refusedWith(t, taskCall(t, d, "TaskCreate", newTask(doc.ID, "n"), nil), "already exists")
	refusedWith(t, taskCall(t, d, "TaskCreate", newTask(doc.ID, "c", blocking("n", "c"), blocking("c", "n")), nil), "blocking cycle")
	refusedWith(t, taskCall(t, d, "TaskCreate", newTask(doc.ID, "u", blocking("ghost", "u")), nil), "invalid edge ghost -> u")
	// FAE-CLI-002: a given priority below 10, 0 included, is refused; only
	// an absent one takes 10 (task a above).
	for _, low := range []int{5, 0} {
		p := newTask(doc.ID, "low")
		p.Priority = &low
		refusedWith(t, taskCall(t, d, "TaskCreate", p, nil), "priority")
		refusedWith(t, taskCall(t, d, "Admit", map[string]any{"id": "low", "workflow_id": doc.ID, "tree": "tree", "content": "do low", "priority": low, "authorizing_message": "message"}, nil), "priority")
	}
	if e := liveEnvelope(t, d, "a"); e.Admission.Priority != 10 {
		t.Fatal("an absent priority takes", e.Admission.Priority)
	}
	refusedWith(t, taskCall(t, d, "TaskCreate", newTask(doc.ID, "v", blocking("a", "n")), nil), "does not name the new task")
	refusedWith(t, taskCall(t, d, "TaskCreate", newTask(doc.ID, "z", blocking("z", "a")), nil), "accept-in-flight")
	found, err := d.store.ExistingJobs(ctx, []string{"c", "u", "low", "v", "z"})
	if err != nil {
		t.Fatal(err)
	}
	for id, stored := range found {
		if stored {
			t.Fatal("a refused create wrote task", id)
		}
	}
	accepted := newTask(doc.ID, "z", blocking("z", "a"))
	accepted.AcceptInFlight = true
	if err = taskCall(t, d, "TaskCreate", accepted, nil); err != nil {
		t.Fatal(err)
	}
	// An edge added to the running task with the operator's acceptance is
	// in its TaskGet edges and edits and in BacklogShow; so is its removal.
	if err = taskCall(t, d, "TaskCreate", newTask(doc.ID, "w"), nil); err != nil {
		t.Fatal(err)
	}
	edge := BacklogEdgeParams{BacklogEdge: blocking("w", "a"), AcceptInFlight: true, AuthorizingMessage: "message"}
	if err = taskCall(t, d, "BacklogEdge", edge, nil); err != nil {
		t.Fatal(err)
	}
	inbound := func(v TaskView) []string {
		ids := []string{}
		for _, e := range v.Edges.In {
			ids = append(ids, e.From)
		}
		return ids
	}
	shownEdges := func() []controlplane.BacklogEdge {
		var shown struct {
			Edges []controlplane.BacklogEdge `json:"edges"`
		}
		if err := taskCall(t, d, "BacklogShow", map[string]any{}, &shown); err != nil {
			t.Fatal(err)
		}
		return shown.Edges
	}
	v := taskView(t, d, "a", false)
	if !reflect.DeepEqual(inbound(v), []string{"w", "z"}) || len(v.Edges.Out) != 1 || v.Edges.Out[0].To != "n" || !v.InFlight || v.Stage != "work" || !slices.Contains(shownEdges(), blocking("w", "a")) {
		t.Fatalf("running task's edges %+v in-flight=%v stage=%s", v.Edges, v.InFlight, v.Stage)
	}
	edge.Remove, edge.AcceptInFlight = true, false
	if err = taskCall(t, d, "BacklogEdge", edge, nil); err != nil {
		t.Fatal(err)
	}
	v = taskView(t, d, "a", false)
	kinds := []string{}
	for _, e := range v.Edits {
		kinds = append(kinds, e.Kind+":"+string(e.Detail))
	}
	// a's edits: n's create joined a -> n, z's create z -> a, then w -> a
	// added and removed.
	if !reflect.DeepEqual(inbound(v), []string{"z"}) || slices.Contains(shownEdges(), blocking("w", "a")) || len(v.Edits) != 4 || !strings.Contains(kinds[0], `"to":"n"`) || !strings.Contains(kinds[1], `"from":"z"`) || !strings.Contains(kinds[2], `"from":"w","kind":"blocking","removed":false`) || !strings.Contains(kinds[3], `"from":"w","kind":"blocking","removed":true`) {
		t.Fatalf("edge removal or edits %v %v", inbound(v), kinds)
	}
	close(x.release)
	awaitState(t, d, "a", "Done")
	awaitState(t, d, "n", "Done")
	if v = taskView(t, d, "n", false); len(v.Trace) < 3 || v.Trace[0].Stage != "Pending" || v.Trace[1].Stage != "work" || v.Trace[len(v.Trace)-1].Stage != "Done" || v.Trace[len(v.Trace)-1].LeftAt != nil || v.Trace[0].LeftAt == nil {
		t.Fatalf("trace %+v", v.Trace)
	}
}

// TaskUpdate (Dolt): an edit of a task with a turn in flight reaches its
// next stage's brief, not the turn in flight, and is recorded as a
// task-edited event that History and TaskGet show; the structured fields
// likewise apply from the next dispatch while the turn in flight keeps the
// ones it was dispatched under; pinned fields and a stale version are
// refused; an edit that changes nothing writes nothing; a terminal task is
// refused.
func TestTaskUpdateAppliesFromTheNextStage(t *testing.T) {
	cfg, ctx := fixture(t)
	ok := engine.ScriptStep{Output: json.RawMessage(`{"ok":true}`)}
	x := &controlGateExecutor{started: make(chan engine.Dispatch, 100), release: make(chan struct{}), delegate: engine.NewScripted(map[string][]engine.ScriptStep{"work": {ok}, "review": {ok}})}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := twoStageWorkflow()
	installWorkflow(t, d, doc)
	p := newTask(doc.ID, "t")
	p.Content, p.Title = "first acceptance", "T1"
	p.DeclaredFiles, p.TestFilter, p.TestShape = []string{"old_test.go"}, "^Old", "go-unit"
	if err = taskCall(t, d, "TaskCreate", p, nil); err != nil {
		t.Fatal(err)
	}
	first := startedDispatch(t, ctx, x.started)
	root := reviewPhysical(t)
	repo := filepath.Join(root, "repo")
	if err = os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	liveGit(t, repo, "init")
	for _, f := range []string{"old_test.go", "new_test.go", "docs/a.md"} {
		full := filepath.Join(repo, f)
		if err = os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(full, []byte("package sample\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	liveGit(t, repo, "add", ".")
	liveGit(t, repo, "commit", "-m", "base")
	manager := &worktree.Manager{Repository: repo, Root: filepath.Join(root, "trees")}
	live := &liveExecutor{daemon: d, config: LiveConfig{Base: "HEAD"}, manager: manager, broker: &effects.Broker{Manager: manager}}
	text := func(s string) *string { return &s }
	priority := 30
	var updated struct {
		Changed []string `json:"changed"`
		Effect  string   `json:"effect"`
		Version int64    `json:"version"`
	}
	if err = taskCall(t, d, "TaskUpdate", TaskUpdateParams{ID: "t", Title: text("T2"), Content: text("second acceptance"), Priority: &priority, AuthorizingMessage: "message"}, &updated); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updated.Changed, []string{"title", "content", "priority"}) || updated.Effect != "from-next-stage" {
		t.Fatalf("update %+v", updated)
	}
	// A structured edit during the turn is stored for the next dispatch.
	files, filter, shape := []string{"docs/a.md", "new_test.go"}, "^check", "script"
	var structured struct {
		Changed []string `json:"changed"`
		Effect  string   `json:"effect"`
	}
	if err = taskCall(t, d, "TaskUpdate", TaskUpdateParams{ID: "t", DeclaredFiles: &files, TestFilter: &filter, TestShape: &shape, AuthorizingMessage: "message"}, &structured); err != nil || !reflect.DeepEqual(structured.Changed, []string{"declared_files", "test_filter", "test_shape"}) || structured.Effect != "from-next-stage" {
		t.Fatal("structured edit in flight", structured, err)
	}
	bad := []string{"../outside"}
	refusedWith(t, taskCall(t, d, "TaskUpdate", TaskUpdateParams{ID: "t", DeclaredFiles: &bad, AuthorizingMessage: "message"}, nil), "declared file")
	refusedWith(t, taskCall(t, d, "TaskUpdate", TaskUpdateParams{ID: "t", WorkflowID: text("other"), AuthorizingMessage: "message"}, nil), "pinned")
	refusedWith(t, taskCall(t, d, "TaskUpdate", TaskUpdateParams{ID: "t", ExpectedVersion: 1, Title: text("T3"), AuthorizingMessage: "message"}, nil), "not the expected 1")
	var unchanged struct {
		Changed []string `json:"changed"`
		Version int64    `json:"version"`
	}
	if err = taskCall(t, d, "TaskUpdate", TaskUpdateParams{ID: "t", Title: text("T2"), DeclaredFiles: &files, WorkflowID: text(doc.ID), AuthorizingMessage: "message"}, &unchanged); err != nil || len(unchanged.Changed) != 0 || unchanged.Version != updated.Version+1 {
		t.Fatal("a no-op update wrote", unchanged, err)
	}
	e := liveEnvelope(t, d, "t")
	if flight := acceptanceContext(e, first); !strings.Contains(flight, "first acceptance") || strings.Contains(flight, "second") {
		t.Fatal("the turn in flight saw the edit", flight)
	}
	// The turn in flight runs under the fields it was dispatched with; the
	// task (and so its next dispatch and TaskGet) carries the new ones.
	if flight := dispatchAdmission(e, first); !reflect.DeepEqual(flight.DeclaredFiles, []string{"old_test.go"}) || flight.TestFilter != "^Old" || flight.TestShape != "go-unit" || !reflect.DeepEqual(e.Admission.DeclaredFiles, files) || e.Admission.TestFilter != filter || e.Admission.TestShape != shape || e.SupersededFields == nil || e.SupersededFields.Until != e.Job.Sequence+1 {
		t.Fatalf("the turn in flight saw the structured edit: %+v stored %+v", flight, e.SupersededFields)
	}
	if v := taskView(t, d, "t", false); !reflect.DeepEqual(v.DeclaredFiles, files) || v.TestFilter != filter || v.TestShape != shape || v.SupersededFields == nil || !reflect.DeepEqual(v.SupersededFields.DeclaredFiles, []string{"old_test.go"}) || v.SupersededFields.TestFilter != "^Old" || v.SupersededFields.TestShape != "go-unit" || v.SupersededFields.Dispatch != first.ID {
		t.Fatalf("TaskGet lacks the pending structured edit or superseded_fields: %+v", v)
	}
	if s, err := live.dispatchLiveScope(first); err != nil || !reflect.DeepEqual(s.Files, []string{"old_test.go"}) {
		t.Fatalf("dispatchLiveScope in flight: %+v %v", s, err)
	}
	binding, err := live.resolve(ctx, first)
	if err != nil || !reflect.DeepEqual(binding.Scope.Files, []string{"old_test.go"}) || !strings.Contains(binding.OperatorContext, "first acceptance") || strings.Contains(binding.OperatorContext, "second") {
		t.Fatalf("live resolve in flight: %+v %v", binding, err)
	}
	tree, err := live.broker.Tree(ctx, live.bindingPath(binding.Worktree))
	if err != nil {
		t.Fatal(err)
	}
	changeFirst := first
	changeFirst.Tree = tree
	changeFirst.Config = workflow.Stage{Phase: "test-first", ChangeProducer: true}
	if err = os.WriteFile(filepath.Join(binding.Worktree, "old_test.go"), []byte(liveTestSource), 0600); err != nil {
		t.Fatal(err)
	}
	if err = live.checkChange(ctx, changeFirst, binding); err != nil {
		t.Fatalf("in-flight checkChange refused superseded file: %v", err)
	}
	liveGit(t, binding.Worktree, "checkout", "--", ".")
	if err = os.WriteFile(filepath.Join(binding.Worktree, "new_test.go"), []byte(liveTestSource), 0600); err != nil {
		t.Fatal(err)
	}
	if err = live.checkChange(ctx, changeFirst, binding); err == nil || !strings.Contains(err.Error(), "outside declared files") {
		t.Fatalf("in-flight checkChange accepted next-stage file: %v", err)
	}
	liveGit(t, binding.Worktree, "checkout", "--", ".")
	close(x.release)
	next := startedDispatch(t, ctx, x.started)
	if next.Stage != "review" {
		t.Fatal("next dispatch", next.Stage)
	}
	e = liveEnvelope(t, d, "t")
	if brief := acceptanceContext(e, next); !strings.Contains(brief, "second acceptance") || !strings.Contains(brief, controlplane.Digest([]byte("second acceptance"))) || strings.Contains(brief, "first") {
		t.Fatal("the next stage's brief lacks the edit", brief)
	}
	if a := dispatchAdmission(e, next); !reflect.DeepEqual(a.DeclaredFiles, files) || a.TestFilter != filter || a.TestShape != shape {
		t.Fatalf("the next dispatch lacks the structured edit %+v", a)
	}
	if v := taskView(t, d, "t", false); v.SupersededFields != nil {
		t.Fatalf("TaskGet still shows superseded_fields on the next dispatch: %+v", v.SupersededFields)
	}
	if s, err := live.dispatchLiveScope(next); err != nil || !reflect.DeepEqual(s.Files, files) {
		t.Fatalf("dispatchLiveScope on next stage: %+v %v", s, err)
	}
	nextBinding, err := live.resolve(ctx, next)
	if err != nil || !reflect.DeepEqual(nextBinding.Scope.Files, files) || !strings.Contains(nextBinding.OperatorContext, "second acceptance") || strings.Contains(nextBinding.OperatorContext, "first") {
		t.Fatalf("live resolve on next stage: %+v %v", nextBinding, err)
	}
	changeNext := next
	changeNext.Tree = tree
	changeNext.Config = workflow.Stage{Phase: "test-first", ChangeProducer: true}
	if err = os.WriteFile(filepath.Join(nextBinding.Worktree, "new_test.go"), []byte(liveTestSource), 0600); err != nil {
		t.Fatal(err)
	}
	if err = live.checkChange(ctx, changeNext, nextBinding); err != nil {
		t.Fatalf("next-stage checkChange refused updated file: %v", err)
	}
	liveGit(t, nextBinding.Worktree, "checkout", "--", ".")
	if err = os.WriteFile(filepath.Join(nextBinding.Worktree, "old_test.go"), []byte(liveTestSource), 0600); err != nil {
		t.Fatal(err)
	}
	if err = live.checkChange(ctx, changeNext, nextBinding); err == nil || !strings.Contains(err.Error(), "outside declared files") {
		t.Fatalf("next-stage checkChange accepted superseded file: %v", err)
	}
	liveGit(t, nextBinding.Worktree, "checkout", "--", ".")
	awaitState(t, d, "t", "Done")
	var history HistoryPage
	if err = taskCall(t, d, "TaskHistory", HistoryParams{ID: "t", Summary: true, Limit: 500}, &history); err != nil {
		t.Fatal(err)
	}
	edited := []string{}
	for _, ev := range history.Events {
		if ev.Kind == "task-edited" {
			edited = append(edited, string(ev.Payload))
		}
	}
	if len(edited) != 2 || !strings.Contains(edited[0], `"fields":["title","content","priority"]`) || !strings.Contains(edited[0], `"in_flight":"`+first.ID+`"`) || !strings.Contains(edited[1], `"fields":["declared_files","test_filter","test_shape"]`) || !strings.Contains(edited[1], `"effect":"from-next-stage"`) || !strings.Contains(edited[1], `"in_flight":"`+first.ID+`"`) {
		t.Fatal("history lacks the task-edited events", edited)
	}
	v := taskView(t, d, "t", false)
	if v.Title != "T2" || v.Content != "second acceptance" || v.Priority != 30 || len(v.Edits) != 2 || v.Edits[0].Kind != "task-edited" || v.Edits[1].Kind != "task-edited" || v.EditsTruncated {
		t.Fatalf("TaskGet %+v", v)
	}
	work := slices.IndexFunc(v.Trace, func(x TaskTraceEntry) bool { return x.Stage == "work" })
	if work < 0 || len(v.Trace[work].Edits) != 2 || v.Trace[work].To != "review" || !strings.HasPrefix(v.Trace[work].Outcome, "completed") {
		t.Fatalf("the edit is not in the visit it was made during: %+v", v.Trace)
	}
	refusedWith(t, taskCall(t, d, "TaskUpdate", TaskUpdateParams{ID: "t", Title: text("late"), AuthorizingMessage: "message"}, nil), "terminal")
	if n := auditCount(t, ctx, d, "TaskUpdate", "operator"); n != 2 {
		t.Fatal("TaskUpdate audits", n)
	}
}

// TaskDelete of tasks that hold no turn (Dolt): a task that never started is
// withdrawn and an Escalated one retired at once; both end Retired with their
// history, events and audit kept and their blocking dependents held, or
// re-pointed to a named successor. A successor that depends on the deleted
// task is refused before anything is written. Structured fields change
// between turns.
func TestTaskDeleteWithdrawsRetiresAndRepoints(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	stopTheDrive(t, d)
	doc := shortWorkflow()
	installWorkflow(t, d, doc)
	for _, p := range []TaskCreateParams{newTask(doc.ID, "p"), newTask(doc.ID, "dep", blocking("p", "dep"))} {
		if err = taskCall(t, d, "TaskCreate", p, nil); err != nil {
			t.Fatal(err)
		}
	}
	files, filter, shape := []string{"docs/a.md", "src/b.txt"}, "^check", "script"
	var updated struct {
		Changed []string `json:"changed"`
		Effect  string   `json:"effect"`
	}
	if err = taskCall(t, d, "TaskUpdate", TaskUpdateParams{ID: "p", DeclaredFiles: &files, TestFilter: &filter, TestShape: &shape, AuthorizingMessage: "message"}, &updated); err != nil || !reflect.DeepEqual(updated.Changed, []string{"declared_files", "test_filter", "test_shape"}) || updated.Effect != "applied" {
		t.Fatal("structured update", updated, err)
	}
	bad := []string{"../outside"}
	refusedWith(t, taskCall(t, d, "TaskUpdate", TaskUpdateParams{ID: "p", DeclaredFiles: &bad, AuthorizingMessage: "message"}, nil), "declared file")
	none := []string{}
	if err = taskCall(t, d, "TaskUpdate", TaskUpdateParams{ID: "p", DeclaredFiles: &none, AuthorizingMessage: "message"}, &updated); err != nil || !reflect.DeepEqual(updated.Changed, []string{"declared_files"}) || len(liveEnvelope(t, d, "p").Admission.DeclaredFiles) != 0 {
		t.Fatal("clearing declared files", updated, err)
	}
	type deleted struct {
		Action     string   `json:"action"`
		Collected  bool     `json:"collected_in_flight"`
		Held       []string `json:"held_dependents"`
		Repointed  []string `json:"repointed_dependents"`
		State      string   `json:"state"`
		Pending    bool     `json:"pending"`
		ReasonKind string   `json:"reason_class"`
	}
	var out deleted
	if err = taskCall(t, d, "TaskDelete", TaskDeleteParams{ID: "p", Reason: "not needed", AuthorizingMessage: "message"}, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, deleted{Action: "withdrawn", Held: []string{"dep"}, Repointed: []string{}, State: "Retired", ReasonKind: "obsolete"}) {
		t.Fatalf("delete %+v", out)
	}
	refusedWith(t, taskCall(t, d, "TaskGet", TaskGetParams{ID: "p"}, nil), "retired task hidden")
	refusedWith(t, taskCall(t, d, "TaskGet", TaskGetParams{ID: "ghost"}, nil), "unknown task ghost")
	v := taskView(t, d, "p", true)
	stages := []string{}
	for _, visit := range v.Trace {
		stages = append(stages, visit.Stage)
	}
	if v.State != "Retired" || !reflect.DeepEqual(stages, []string{"Pending", "Escalated", "Retired"}) || v.Edits[len(v.Edits)-1].Kind != "task-deleted" || len(v.Edits) != 4 || v.PendingDelete != nil {
		t.Fatalf("withdrawn task %s %+v %+v", v.State, stages, v.Edits)
	}
	refusedWith(t, taskCall(t, d, "TaskDelete", TaskDeleteParams{ID: "p", Reason: "again", AuthorizingMessage: "message"}, nil), "finished")
	if v = taskView(t, d, "dep", false); v.Readiness["ready"] != false || !reflect.DeepEqual(v.Readiness["withheld_by"], []any{"p: Retired"}) || *v.Edges.In[0].Satisfied {
		t.Fatalf("held dependent %+v %+v", v.Readiness, v.Edges)
	}
	// Supersession re-points the blocking dependents to the successor.
	for _, p := range []TaskCreateParams{newTask(doc.ID, "q"), newTask(doc.ID, "dep2", blocking("q", "dep2")), newTask(doc.ID, "s")} {
		if err = taskCall(t, d, "TaskCreate", p, nil); err != nil {
			t.Fatal(err)
		}
	}
	out = deleted{}
	if err = taskCall(t, d, "TaskDelete", TaskDeleteParams{ID: "q", Reason: "replaced", SupersededBy: "s", AuthorizingMessage: "message"}, &out); err != nil {
		t.Fatal(err)
	}
	if into, err := d.store.DependentEdges(ctx, "dep2"); err != nil || !reflect.DeepEqual(into, []controlplane.BacklogEdge{blocking("s", "dep2")}) || !reflect.DeepEqual(out.Repointed, []string{"dep2"}) || len(out.Held) != 0 || out.ReasonKind != "superseded-by" {
		t.Fatal("supersession", into, out, err)
	}
	// The moved edge is an edit of the dependent and of the successor.
	for _, id := range []string{"dep2", "s"} {
		v := taskView(t, d, id, false)
		last := v.Edits[len(v.Edits)-1]
		var detail map[string]any
		if json.Unmarshal(last.Detail, &detail) != nil || last.Kind != "backlog_edge_changed" || !reflect.DeepEqual(detail, map[string]any{"from": "s", "to": "dep2", "kind": "blocking", "removed": false, "repointed_from": "q"}) {
			t.Fatalf("re-pointed edge not among %s's edits: %+v", id, v.Edits)
		}
	}
	// A successor depending on the deleted task is refused before any write.
	for _, p := range []TaskCreateParams{newTask(doc.ID, "r"), newTask(doc.ID, "s2", blocking("r", "s2"))} {
		if err = taskCall(t, d, "TaskCreate", p, nil); err != nil {
			t.Fatal(err)
		}
	}
	before := liveEnvelope(t, d, "r")
	version := d.jobs["r"].Version
	refusedWith(t, taskCall(t, d, "TaskDelete", TaskDeleteParams{ID: "r", Reason: "x", SupersededBy: "s2", AuthorizingMessage: "message"}, nil), "depends on r")
	refusedWith(t, taskCall(t, d, "TaskDelete", TaskDeleteParams{ID: "r", Reason: "x", SupersededBy: "ghost", AuthorizingMessage: "message"}, nil), "successor")
	if after := liveEnvelope(t, d, "r"); after.Job.State != domain.Pending || after.Deletion != nil || d.jobs["r"].Version != version || !reflect.DeepEqual(after.Job.History, before.Job.History) {
		t.Fatal("a refused delete wrote", after.Job.State)
	}
	// An Escalated task with nothing in flight is retired at once.
	if _, err = call(t, d, "Escalate", TaskControlParams{ID: "s2", Reason: "operator review", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	out = deleted{}
	if err = taskCall(t, d, "TaskDelete", TaskDeleteParams{ID: "s2", Reason: "dropped", AuthorizingMessage: "message"}, &out); err != nil || out.State != "Retired" || out.Action != "withdrawn" {
		t.Fatal("escalated delete", out, err)
	}
	// p and q took two checkpoints each (escalate, retire), s2 one.
	if n := auditCount(t, ctx, d, "TaskDelete", "operator"); n != 5 {
		t.Fatal("TaskDelete audits", n)
	}
}

// TaskDelete of a task with a turn in flight (Dolt): the delete escalates it,
// which holds the turn; the drive collects the turn (never abandons it) and
// then retires the task. While it waits, a second delete and a restart of
// the task are refused, and TaskGet shows the pending delete. The blocking
// dependent stays held.
func TestTaskDeleteCollectsTheTurnInFlightThenRetires(t *testing.T) {
	cfg, ctx := fixture(t)
	ok := engine.ScriptStep{Output: json.RawMessage(`{"ok":true}`)}
	x := &controlGateExecutor{started: make(chan engine.Dispatch, 100), release: make(chan struct{}), delegate: engine.NewScripted(map[string][]engine.ScriptStep{"work": {ok, ok}})}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := shortWorkflow()
	installWorkflow(t, d, doc)
	for _, p := range []TaskCreateParams{newTask(doc.ID, "busy"), newTask(doc.ID, "after", blocking("busy", "after"))} {
		if err = taskCall(t, d, "TaskCreate", p, nil); err != nil {
			t.Fatal(err)
		}
	}
	first := startedDispatch(t, ctx, x.started)
	var out map[string]any
	if err = taskCall(t, d, "TaskDelete", TaskDeleteParams{ID: "busy", Reason: "stop it", AuthorizingMessage: "message"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["action"] != "retired" || out["collected_in_flight"] != true || out["pending"] != true || out["state"] != "Escalated" || out["dispatch"] != first.ID || !reflect.DeepEqual(out["held_dependents"], []any{"after"}) {
		t.Fatalf("pending delete %v", out)
	}
	refusedWith(t, taskCall(t, d, "TaskDelete", TaskDeleteParams{ID: "busy", Reason: "again", AuthorizingMessage: "message"}, nil), "already being deleted")
	refusedWith(t, taskCall(t, d, "RestartWithInformation", TaskControlParams{ID: "busy", Information: "continue", AuthorizingMessage: "message"}, nil), "being deleted")
	if v := taskView(t, d, "busy", false); v.PendingDelete == nil || v.PendingDelete.Dispatch != first.ID || v.PendingDelete.Status != "waiting-for-turn" || v.PendingDelete.Reason != "stop it" || v.PendingDelete.SupersededBy != "" || v.PendingDelete.RequestedAt.IsZero() || !v.InFlight || v.EscalationKind != domain.OperatorEscalate || v.Stage != "work" || v.EscalatedAt == nil {
		t.Fatalf("pending delete view %+v", v)
	}
	if rows := boardRows(t, d, false); !rows["busy"].PendingDelete || rows["after"].PendingDelete {
		t.Fatalf("board pending_delete %+v", rows)
	}
	close(x.release)
	awaitState(t, d, "busy", "Retired")
	if rows := boardRows(t, d, true); rows["busy"].PendingDelete || rows["busy"].State != "Retired" {
		t.Fatalf("board pending_delete after the retirement %+v", rows["busy"])
	}
	e := liveEnvelope(t, d, "busy")
	if e.Deletion != nil || e.Job.InFlight != nil || len(e.Job.Provenance) != 1 || e.Job.Provenance[0].DispatchID != first.ID || e.Job.Reason != "obsolete stop it" {
		t.Fatalf("the held turn was not collected before retirement: deletion=%v provenance=%d reason=%q", e.Deletion, len(e.Job.Provenance), e.Job.Reason)
	}
	if n := auditCount(t, ctx, d, "TaskDelete", "daemon"); n != 1 {
		t.Fatal("the drive's completion of the delete is not audited", n)
	}
	if v := taskView(t, d, "after", false); v.State != "Pending" || v.Readiness["ready"] != false {
		t.Fatalf("the dependent was released %+v", v.Readiness)
	}
}

// A pending delete survives a restart (Dolt): a held turn interrupted by the
// stop parks UncertainDispatch, the operator's resolution ends it, and the
// drive retires the task; a delete whose task was escalated but not yet
// retired when the daemon stopped completes at the next start.
func TestTaskDeleteSurvivesARestart(t *testing.T) {
	cfg, ctx := fixture(t)
	ok := engine.ScriptStep{Output: json.RawMessage(`{"ok":true}`)}
	script := map[string][]engine.ScriptStep{"work": {ok, ok, ok}}
	x := &controlGateExecutor{started: make(chan engine.Dispatch, 100), release: make(chan struct{}), delegate: engine.NewScripted(script)}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	doc := shortWorkflow()
	installWorkflow(t, d, doc)
	if err = taskCall(t, d, "TaskCreate", newTask(doc.ID, "held"), nil); err != nil {
		t.Fatal(err)
	}
	first := startedDispatch(t, ctx, x.started)
	if err = taskCall(t, d, "TaskDelete", TaskDeleteParams{ID: "held", Reason: "restart between", AuthorizingMessage: "message"}, nil); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	cfg.Executor = engine.NewScripted(script)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	e := liveEnvelope(t, d, "held")
	if e.Deletion == nil || e.Job.State != domain.Escalated || e.Job.InFlight == nil || e.Job.Reason != "UncertainDispatch" {
		t.Fatalf("after restart: deletion=%v state=%s reason=%s", e.Deletion, e.Job.State, e.Job.Reason)
	}
	if v := taskView(t, d, "held", false); v.PendingDelete == nil || v.PendingDelete.Status != "uncertain" || !v.PendingDelete.UncertainDispatch || v.PendingDelete.Reason != "restart between" {
		t.Fatalf("uncertain delete view after restart %+v", v.PendingDelete)
	}
	if err = taskCall(t, d, "ResolveUncertain", ResolveUncertainParams{ID: "held", DispatchID: first.ID, Action: "escalate", AuthorizingMessage: "message"}, nil); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "held", "Retired")
	// A delete escalated but not yet retired at the stop.
	stopTheDrive(t, d)
	if err = taskCall(t, d, "TaskCreate", newTask(doc.ID, "half"), nil); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	s := d.jobs["half"]
	var half envelope
	_ = decodeEnvelope(s.Payload, &half)
	half.Job, _, err = engine.ApplyControl(half.Job, engine.Control{Method: "Escalate", Reason: "TaskDelete: crash between"}, time.Now())
	if err == nil {
		half.Deletion = &pendingTaskDelete{Retirement: "obsolete crash between", Action: "withdrawn", RequestedAt: time.Now().UTC(), Actor: "operator", MessageID: "message"}
		s, err = d.persist(ctx, s, half, nil)
		d.jobs[s.ID] = s
	}
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// Nothing is in flight, so the delete is retiring: the next round ends it.
	if v := taskView(t, d, "half", false); v.PendingDelete == nil || v.PendingDelete.Status != "retiring" || v.PendingDelete.Reason != "crash between" {
		t.Fatalf("retiring delete view %+v", v.PendingDelete)
	}
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "half", "Retired")
	if e = liveEnvelope(t, d, "half"); e.Deletion != nil || e.Job.Reason != "obsolete crash between" {
		t.Fatal("restart completion", e.Deletion, e.Job.Reason)
	}
}

// FAE-API-012 (Dolt): the live task writes replay under an idempotence key,
// returning the original result and writing nothing; the same key with
// another request is refused.
func TestTaskVerbsReplayUnderAnIdempotenceKey(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	stopTheDrive(t, d)
	doc := shortWorkflow()
	installWorkflow(t, d, doc)
	title := "edited"
	for _, step := range []struct {
		method, key string
		params      any
	}{
		{"TaskCreate", "create-k", newTask(doc.ID, "k")},
		{"TaskUpdate", "update-k", TaskUpdateParams{ID: "k", Title: &title, AuthorizingMessage: "message"}},
		{"TaskDelete", "delete-k", TaskDeleteParams{ID: "k", Reason: "done with it", AuthorizingMessage: "message"}},
	} {
		first, err := call(t, d, step.method, step.params, step.key)
		if err != nil {
			t.Fatal(step.method, err)
		}
		version := d.jobs["k"].Version
		again, err := call(t, d, step.method, step.params, step.key)
		if err != nil || string(again.Result) != string(first.Result) || d.jobs["k"].Version != version {
			t.Fatalf("%s replay: %s vs %s, %v", step.method, again.Result, first.Result, err)
		}
	}
	other := "other"
	refusedWith(t, taskCall(t, d, "TaskUpdate", TaskUpdateParams{ID: "k", Title: &other, AuthorizingMessage: "message"}, nil), "terminal")
	if _, err = call(t, d, "TaskUpdate", TaskUpdateParams{ID: "k", Title: &other, AuthorizingMessage: "message"}, "update-k"); err == nil {
		t.Fatal("a reused key with another request was accepted")
	}
	for action, want := range map[string]int{"TaskCreate": 1, "TaskUpdate": 1, "TaskDelete": 2} {
		if n := auditCount(t, ctx, d, action, "operator"); n != want {
			t.Fatalf("%s audits %d want %d", action, n, want)
		}
	}
}

// editTask's rules without a plane: the changed fields in order; content
// supersession for the turn in flight; the structured fields validated, and
// superseded for the turn in flight so they apply from the next dispatch; a
// pinned change refused; the task-edited event only when a field changed.
func TestEditTaskRules(t *testing.T) {
	j, err := engine.New("e", 1, shortWorkflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	e := envelope{Job: j, Admission: AdmitParams{ID: "e", Title: "t", Content: "old", Priority: 10}}
	raw, _ := json.Marshal(e)
	old := controlplane.JobSnapshot{ID: "e", Version: 3, State: string(j.State), Payload: raw}
	text := func(s string) *string { return &s }
	audit := controlplane.Audit{Actor: "operator", MessageID: "m"}
	got, err := editTask(old, e, controlplane.BacklogItem{ID: "e"}, taskEdit{Title: text("t"), Content: text("new"), Source: text("ref")}, audit, "TaskUpdate")
	if err != nil || !reflect.DeepEqual(got.changed, []string{"content", "source_ref"}) || got.effect != "applied" || len(got.snapshot.Events) != 1 || got.snapshot.Events[0].Kind != "task-edited" || got.item.Source != "ref" || got.item.AcceptanceDigest != controlplane.Digest([]byte("new")) {
		t.Fatalf("edit %+v %v", got, err)
	}
	if got, err = editTask(old, e, controlplane.BacklogItem{ID: "e"}, taskEdit{Title: text("t")}, audit, "TaskUpdate"); err != nil || len(got.changed) != 0 || got.snapshot.Events != nil {
		t.Fatal("a no-op edit carries an event", got.changed, err)
	}
	for _, bad := range []taskEdit{{Content: text("  ")}, {Priority: func() *int { n := 9; return &n }()}, {TestShape: text("nope")}, {TestFilter: text("(")}, {TestFilter: text(`\Q/\E`)}, {TestFilter: text(`x\Qa|b\E`)}} {
		if _, err = editTask(old, e, controlplane.BacklogItem{ID: "e"}, bad, audit, "TaskUpdate"); err == nil {
			t.Fatalf("invalid edit accepted %+v", bad)
		}
	}
	e.Job.InFlight = &engine.Dispatch{ID: "e/h/4", Stage: "work"}
	e.Job.Sequence = 4
	e.Admission.TestFilter = "^old"
	files := []string{"a.txt"}
	got, err = editTask(old, e, controlplane.BacklogItem{ID: "e"}, taskEdit{DeclaredFiles: &files, TestShape: text("script")}, audit, "TaskUpdate")
	var saved envelope
	if err == nil {
		err = decodeEnvelope(got.snapshot.Payload, &saved)
	}
	// The structured edit is stored for the next dispatch (sequence 5); the
	// turn in flight (sequence 4) keeps the fields it was dispatched under.
	if err != nil || got.effect != "from-next-stage" || !reflect.DeepEqual(got.changed, []string{"declared_files", "test_shape"}) || saved.SupersededFields == nil || saved.SupersededFields.Until != 5 || len(saved.SupersededFields.DeclaredFiles) != 0 || saved.SupersededFields.TestFilter != "^old" || saved.SupersededFields.TestShape != "" {
		t.Fatalf("in-flight structured edit %+v %+v %v", got.changed, saved.SupersededFields, err)
	}
	inFlight, next := engine.Dispatch{ID: "e/h/4"}, engine.Dispatch{ID: "e/h/5"}
	if a := dispatchAdmission(saved, inFlight); len(a.DeclaredFiles) != 0 || a.TestShape != "" || a.TestFilter != "^old" {
		t.Fatalf("the turn in flight sees the edit %+v", a)
	}
	if a := dispatchAdmission(saved, next); !reflect.DeepEqual(a.DeclaredFiles, files) || a.TestShape != "script" || a.TestFilter != "^old" {
		t.Fatalf("the next dispatch lacks the edit %+v", a)
	}
	// A second edit during the same turn keeps the fields the turn started with.
	again, err := editTask(got.snapshot, saved, controlplane.BacklogItem{ID: "e"}, taskEdit{TestFilter: text("^new")}, audit, "TaskUpdate")
	var twice envelope
	if err == nil {
		err = decodeEnvelope(again.snapshot.Payload, &twice)
	}
	if err != nil || twice.SupersededFields.TestFilter != "^old" || twice.SupersededFields.Until != 5 || dispatchAdmission(twice, inFlight).TestFilter != "^old" || dispatchAdmission(twice, next).TestFilter != "^new" || len(dispatchAdmission(twice, inFlight).DeclaredFiles) != 0 {
		t.Fatalf("second in-flight edit %+v %v", twice.SupersededFields, err)
	}
	if !dispatchBefore(engine.Dispatch{ID: "no-sequence"}, 1) || dispatchBefore(next, 5) || !dispatchBefore(inFlight, 5) {
		t.Fatal("dispatchBefore")
	}
	got, err = editTask(old, e, controlplane.BacklogItem{ID: "e"}, taskEdit{Content: text("newer")}, audit, "TaskUpdate")
	var content envelope
	if err == nil {
		err = decodeEnvelope(got.snapshot.Payload, &content)
	}
	if err != nil || got.effect != "from-next-stage" || content.Superseded == nil || content.Superseded.Content != "old" || content.Superseded.Until != 5 || content.SupersededFields != nil {
		t.Fatalf("in-flight content edit %+v %v", content.Superseded, err)
	}
	if !pinnedEdit(TaskUpdateParams{WorkflowID: text("other")}, controlplane.JobSnapshot{WorkflowID: "short"}, e) || pinnedEdit(TaskUpdateParams{WorkflowID: text("short")}, controlplane.JobSnapshot{WorkflowID: "short"}, e) {
		t.Fatal("pinned edit check")
	}
}

// The trace from transitions without a plane: the first visit starts at
// admission, each transition ends one visit, and an edit lands in the visit
// it was made during; the current stage of an Escalated task is the stage
// it was escalated from.
func TestTaskTraceAndCurrentStage(t *testing.T) {
	at := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	transitions := []controlplane.TransitionRecord{
		{From: "Pending", To: "work", Note: "released", Attempt: 1, CreatedAt: at.Add(time.Minute)},
		{From: "work", To: "Escalated", Note: "OperatorEscalate: TaskDelete: x", Attempt: 1, CreatedAt: at.Add(3 * time.Minute)},
	}
	edits := []TaskEditEntry{{Kind: "task-edited", At: at.Add(2 * time.Minute)}, {Kind: "task-edited", At: at.Add(30 * time.Second)}}
	trace := taskTrace(transitions, at, edits)
	if len(trace) != 3 || trace[0].Stage != "Pending" || !trace[0].EnteredAt.Equal(at) || trace[0].To != "work" || trace[1].Outcome != "OperatorEscalate: TaskDelete: x" || trace[2].LeftAt != nil || trace[2].Attempt != 1 {
		t.Fatalf("trace %+v", trace)
	}
	if len(trace[0].Edits) != 1 || len(trace[1].Edits) != 1 || len(trace[2].Edits) != 0 {
		t.Fatalf("edits per visit %+v", trace)
	}
	j, err := engine.New("e", 1, shortWorkflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	j.State = domain.Escalated
	if stage := currentStage(envelope{Job: j}, transitions); stage != "work" {
		t.Fatal("current stage", stage)
	}
	if trace = taskTrace(nil, at, nil); len(trace) != 1 || trace[0].Stage != "Pending" || trace[0].LeftAt != nil {
		t.Fatalf("a new task's trace %+v", trace)
	}
}

// boardRows reads the Board by task id.
func boardRows(t *testing.T, d *Daemon, includeRetired bool) map[string]BoardItem {
	t.Helper()
	var page BoardPage
	if err := taskCall(t, d, "Board", BoardParams{IncludeRetired: includeRetired, Limit: 500}, &page); err != nil {
		t.Fatal(err)
	}
	rows := map[string]BoardItem{}
	for _, item := range page.Items {
		rows[item.ID] = item
	}
	return rows
}

// taskGateExecutor holds each task's turns until that task is released.
type taskGateExecutor struct {
	started  chan engine.Dispatch
	mu       sync.Mutex
	gates    map[string]chan struct{}
	delegate *engine.Scripted
}

func (x *taskGateExecutor) gate(task string) chan struct{} {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.gates == nil {
		x.gates = map[string]chan struct{}{}
	}
	if _, ok := x.gates[task]; !ok {
		x.gates[task] = make(chan struct{})
	}
	return x.gates[task]
}

func (x *taskGateExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	select {
	case x.started <- d:
	case <-ctx.Done():
		return engine.Result{}, ctx.Err()
	}
	select {
	case <-x.gate(d.TaskID):
	case <-ctx.Done():
		return engine.Result{}, ctx.Err()
	}
	return x.delegate.Execute(ctx, d)
}

// awaitEnvelope polls a task's stored envelope until ok accepts it, for at
// most wait.
func awaitEnvelope(t *testing.T, d *Daemon, id string, wait time.Duration, ok func(envelope) bool) envelope {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		e := liveEnvelope(t, d, id)
		if ok(e) {
			return e
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s did not reach the awaited envelope within %s: state=%s in_flight=%v deletion=%+v", id, wait, e.Job.State, e.Job.InFlight != nil, e.Deletion)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A collected delete retires its task even when the tasks-in-flight cap is
// full and the deleted task is ordered after both a Done task and another
// ready task at the same priority (Dolt): with a cap of 2, done (priority 10),
// c (priority 10, blocked on done) and busy (priority 10) are admitted in
// that order; once done finishes, a (priority 30) takes its slot, and b
// (priority 20) and c (priority 10, ordered before busy by admission time)
// wait for the cap. Once busy's turn is collected, b takes the freed slot
// before the round reaches done, c or busy, and busy still retires in that
// round instead of waiting behind the cap.
func TestTaskDeleteRetiresUnderTheTasksInFlightCap(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.MaxInFlight = 2
	ok := engine.ScriptStep{Output: json.RawMessage(`{"ok":true}`)}
	x := &taskGateExecutor{started: make(chan engine.Dispatch, 100), delegate: engine.NewScripted(map[string][]engine.ScriptStep{"work": {ok, ok, ok, ok, ok}})}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := shortWorkflow()
	installWorkflow(t, d, doc)
	for _, p := range []TaskCreateParams{newTask(doc.ID, "done"), newTask(doc.ID, "c", blocking("done", "c")), newTask(doc.ID, "busy")} {
		if err = taskCall(t, d, "TaskCreate", p, nil); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		startedDispatch(t, ctx, x.started)
	}
	// a and b have higher priority; once done finishes, a takes its slot and
	// both b (priority 20) and c (priority 10, admitted before busy) wait for
	// the cap.
	for id, priority := range map[string]int{"a": 30, "b": 20} {
		p := newTask(doc.ID, id)
		p.Priority = &priority
		if err = taskCall(t, d, "TaskCreate", p, nil); err != nil {
			t.Fatal(err)
		}
	}
	close(x.gate("done"))
	awaitState(t, d, "done", "Done")
	if next := startedDispatch(t, ctx, x.started); next.TaskID != "a" {
		t.Fatal("slot after done went to", next.TaskID)
	}
	awaitEnvelope(t, d, "b", 30*time.Second, func(e envelope) bool { return e.Job.State == "work" && e.Job.InFlight == nil })
	if err = taskCall(t, d, "TaskDelete", TaskDeleteParams{ID: "busy", Reason: "stop it", AuthorizingMessage: "message"}, nil); err != nil {
		t.Fatal(err)
	}
	close(x.gate("busy"))
	// b fills the slot busy's turn freed; the round then visits done (Done),
	// c (withheld at the cap) and busy (collected delete).
	if third := startedDispatch(t, ctx, x.started); third.TaskID != "b" {
		t.Fatal("the freed slot went to", third.TaskID)
	}
	awaitEnvelope(t, d, "busy", 5*time.Second, func(e envelope) bool { return e.Job.State == domain.Retired })
	if e := liveEnvelope(t, d, "busy"); e.Deletion != nil || e.Job.Reason != "obsolete stop it" || len(e.Job.Provenance) != 1 {
		t.Fatalf("retired under the cap: deletion=%v reason=%q provenance=%d", e.Deletion, e.Job.Reason, len(e.Job.Provenance))
	}
	if e := liveEnvelope(t, d, "c"); e.Job.InFlight != nil || e.Job.State == domain.Done {
		t.Fatalf("c ran while the cap was full: state=%s in_flight=%v", e.Job.State, e.Job.InFlight)
	}
	for _, id := range []string{"a", "b"} {
		if e := liveEnvelope(t, d, id); e.Job.InFlight == nil {
			t.Fatal(id, "was not kept in flight")
		}
		close(x.gate(id))
	}
	if fourth := startedDispatch(t, ctx, x.started); fourth.TaskID != "c" {
		t.Fatal("c dispatch", fourth.TaskID)
	}
	close(x.gate("c"))
	awaitState(t, d, "a", "Done")
	awaitState(t, d, "b", "Done")
	awaitState(t, d, "c", "Done")
}

// A delete that fails once its turn is collected (Dolt): the successor was
// made a dependent of the task meanwhile, so the retirement is refused; the
// task stays Escalated with a task-delete-failed event and alert, TaskGet
// shows the delete as failed with its reason and cause, and the Board no
// longer flags it. TaskGet keeps the newest edits and flags the truncation.
func TestTaskDeleteFailureIsShownAndEditsKeepTheNewest(t *testing.T) {
	cfg, ctx := fixture(t)
	ok := engine.ScriptStep{Output: json.RawMessage(`{"ok":true}`)}
	x := &taskGateExecutor{started: make(chan engine.Dispatch, 100), delegate: engine.NewScripted(map[string][]engine.ScriptStep{"work": {ok, ok, ok}})}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := shortWorkflow()
	installWorkflow(t, d, doc)
	for _, p := range []TaskCreateParams{newTask(doc.ID, "doomed"), newTask(doc.ID, "succ")} {
		if err = taskCall(t, d, "TaskCreate", p, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Both run; the successor's turn stays open until the end.
	for _, id := range []string{"doomed", "succ"} {
		if started := startedDispatch(t, ctx, x.started); started.TaskID != id {
			t.Fatal("dispatch order", started.TaskID)
		}
	}
	title := "renamed"
	if err = taskCall(t, d, "TaskUpdate", TaskUpdateParams{ID: "doomed", Title: &title, AuthorizingMessage: "message"}, nil); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = taskCall(t, d, "TaskDelete", TaskDeleteParams{ID: "doomed", Reason: "replaced", SupersededBy: "succ", AuthorizingMessage: "message"}, &out); err != nil || out["pending"] != true {
		t.Fatal("pending delete", out, err)
	}
	requested := taskView(t, d, "doomed", false).PendingDelete
	if requested == nil || requested.Status != "waiting-for-turn" || requested.SupersededBy != "succ" || requested.Reason != "" {
		t.Fatalf("pending supersession view %+v", requested)
	}
	// The successor now depends on the deleted task, which the retirement refuses.
	if err = taskCall(t, d, "BacklogEdge", BacklogEdgeParams{BacklogEdge: blocking("doomed", "succ"), AcceptInFlight: true, AuthorizingMessage: "message"}, nil); err != nil {
		t.Fatal(err)
	}
	close(x.gate("doomed"))
	e := awaitEnvelope(t, d, "doomed", 45*time.Second, func(e envelope) bool { return e.Deletion == nil && e.Job.InFlight == nil })
	if e.Job.State != domain.Escalated {
		t.Fatal("a failed delete left the task", e.Job.State)
	}
	v := taskView(t, d, "doomed", false)
	if v.PendingDelete == nil || v.PendingDelete.Status != "failed" || v.PendingDelete.SupersededBy != "succ" || !strings.Contains(v.PendingDelete.Cause, "depends on doomed") || !v.PendingDelete.RequestedAt.Equal(requested.RequestedAt) || v.PendingDelete.Retirement != "superseded-by succ" {
		t.Fatalf("failed delete view %+v", v.PendingDelete)
	}
	if last := v.Edits[len(v.Edits)-1]; last.Kind != "task-delete-failed" || len(v.Edits) != 3 || v.EditsTruncated {
		t.Fatalf("edits %+v", v.Edits)
	}
	if rows := boardRows(t, d, false); rows["doomed"].PendingDelete || rows["doomed"].State != "Escalated" {
		t.Fatalf("board flags a failed delete %+v", rows["doomed"])
	}
	// With room for two edits, the newest two are listed and the truncation flagged.
	defer func(n int) { taskGetEditLimit = n }(taskGetEditLimit)
	taskGetEditLimit = 2
	v = taskView(t, d, "doomed", false)
	if !v.EditsTruncated || len(v.Edits) != 2 || v.Edits[0].Kind != "backlog_edge_changed" || v.Edits[1].Kind != "task-delete-failed" || v.PendingDelete == nil || v.PendingDelete.Status != "failed" {
		t.Fatalf("truncated edits %v %+v", v.EditsTruncated, v.Edits)
	}
	// The operator can delete it again (obsolete), which retires it.
	taskGetEditLimit = 1000
	if err = taskCall(t, d, "TaskDelete", TaskDeleteParams{ID: "doomed", Reason: "give up", AuthorizingMessage: "message"}, &out); err != nil || out["state"] != "Retired" {
		t.Fatal("second delete", out, err)
	}
	if v = taskView(t, d, "doomed", true); v.PendingDelete != nil {
		t.Fatalf("a retired task shows a delete %+v", v.PendingDelete)
	}
	close(x.gate("succ"))
	awaitState(t, d, "succ", "Done")
}

type bufferLog struct{ bytes.Buffer }

func (b *bufferLog) Close() error { return nil }

// A refusal's line follows its row (FAE-OBS-006): a commit that fails leaves
// no line naming the refusal; one that succeeds is followed by the line.
func TestLogAfterCommitWritesNoLineForAFailedCommit(t *testing.T) {
	log := &bufferLog{}
	d := &Daemon{log: log}
	if err := d.logAfterCommit(func() error { return errors.New("plane down") }, "task a task-delete-refused: x\n"); err == nil || strings.Contains(log.String(), "task-delete-refused") {
		t.Fatalf("a failed commit left a line: %v %q", err, log.String())
	}
	if err := d.logAfterCommit(func() error { return nil }, "task a task-delete-refused: x\n"); err != nil || !strings.Contains(log.String(), "task a task-delete-refused: x") {
		t.Fatalf("a committed row has no line: %v %q", err, log.String())
	}
}

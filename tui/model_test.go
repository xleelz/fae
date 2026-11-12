package tui

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xleelz/fae/tui/client"
)

func TestInitReadsEverythingOffTheUpdateLoop(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{}, d)
	h.m.Init()
	if got := h.queued(); !reflect.DeepEqual(got, []string{"refresh"}) {
		t.Fatalf("Init queued %v, want one refresh", got)
	}
	if len(d.called("Status", "Board", "BacklogShow", "Topology", "Alerts", "Inbox")) != 0 {
		t.Fatal("Init read the daemon on the update loop")
	}
	if len(h.ticks) != 2 || h.ticks[0] != (tickMsg{}) || h.ticks[1] != (clockMsg{}) {
		t.Fatalf("Init scheduled %v, want the poll and the clock", h.ticks)
	}
	h.contains("connecting…")
	h.drain()
	for _, method := range []string{"Status", "Board", "BacklogShow", "Topology", "Alerts", "Inbox"} {
		if len(d.called(method)) != 1 {
			t.Fatalf("one refresh read %s %d times", method, len(d.called(method)))
		}
	}
	h.contains("daemon pid 4242", "snapshot 1.5s old", "read-only")
}

func TestPollingNeverOverlapsAReadInFlight(t *testing.T) {
	h := newHarness(t, Options{}, fixture())
	h.m.Init()
	h.send(tickMsg{})
	h.send(tickMsg{})
	if got := h.queued(); !reflect.DeepEqual(got, []string{"refresh"}) {
		t.Fatalf("ticks during a refresh queued %v", got)
	}
	if len(h.ticks) != 4 {
		t.Fatalf("each tick must schedule the next: %d ticks", len(h.ticks))
	}
	h.drain()
	h.send(tickMsg{})
	if got := h.queued(); !reflect.DeepEqual(got, []string{"refresh"}) {
		t.Fatalf("a tick after the refresh queued %v", got)
	}
}

func TestMoleculesViewShowsStagesInNormalOrderWithTheirTasks(t *testing.T) {
	h := newHarness(t, Options{}, fixture()).start()
	h.contains("Molecule m1", "workflow default rev 3", "normal-case roles: spec → coder → reviewer", "▶ parser", "in stage implement (turn in flight)")
	order := []int{h.index("● Pending"), h.index("● spec"), h.index("● implement"), h.index("● review"), h.index("● Done"), h.index("off the normal path"), h.index("○ Escalated")}
	if !slices.IsSorted(order) {
		t.Fatalf("stages out of normal-case order: %v\n%s", order, h.screen())
	}
	h.contains("! docs", "Escalated AttemptCap: attempt limit 3 reached", "(2 other stages hold no task)")
	h.lacks("○ fix", "○ Blocked")
	// z hides the empty stages of the normal path.
	h.press("z")
	h.contains("● implement", "(4 empty stages on the normal path hidden; z shows them)")
	h.lacks("● spec", "● review")
}

func TestEscalationTextDoesNotRepeatTheKind(t *testing.T) {
	d := fixture()
	d.board.Tasks[4].EscalationKind, d.board.Tasks[4].Reason = "OperatorEscalate", "OperatorEscalate: needs a human"
	d.inbox.Escalated[0].Reason = "OperatorEscalate: needs a human"
	h := newHarness(t, Options{}, d).start()
	h.contains("Escalated OperatorEscalate: needs a human")
	h.lacks("OperatorEscalate: OperatorEscalate")
	h.press("3")
	h.contains("docs  OperatorEscalate  since 2h00m ago  needs a human")
	for _, c := range [][3]string{{"", "", ""}, {"K", "", "K"}, {"", "r", "r"}, {"K", "K", "K"}, {"K", "K: r", "K: r"}, {"K", "r", "K: r"}} {
		if got := escalation(c[0], c[1]); got != c[2] {
			t.Fatalf("escalation(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

// A pending delete is read from the Board's stored flag and TaskGet's
// pending_delete object, never guessed from the escalation reason: a delete
// pending after a restart (reason UncertainDispatch or status uncertain) says
// to run resolve-uncertain --action escalate, an active turn in flight says it
// retires once collected, a plain escalation whose reason starts "TaskDelete:"
// is not marked, and a failed delete says so.
func TestAPendingDeleteIsReadFromTheStoredFieldNotTheReasonText(t *testing.T) {
	d := fixture()
	docs := &d.board.Tasks[4]
	docs.EscalationKind, docs.Reason, docs.PendingDelete = "OperatorEscalate", "UncertainDispatch", true
	d.inbox.Escalated[0].Reason = "UncertainDispatch"
	detail := d.details["docs"]
	detail.EscalationKind, detail.EscalationReason = "OperatorEscalate", "UncertainDispatch"
	detail.PendingDelete = &client.PendingDelete{Reason: "replaced", RequestedAt: fixedNow.Add(-time.Hour), Status: "waiting-for-turn"}
	d.details["docs"] = detail
	d.board.Tasks = append(d.board.Tasks,
		client.Task{ID: "waiting", State: "Escalated", Priority: 13, EscalationKind: "OperatorEscalate", Reason: "OperatorEscalate: TaskDelete: obsolete", PendingDelete: true, Version: 3},
		client.Task{ID: "plain", State: "Escalated", Priority: 12, EscalationKind: "OperatorEscalate", Reason: "OperatorEscalate: TaskDelete: not really", Version: 2},
		client.Task{ID: "failed", State: "Escalated", Priority: 11, EscalationKind: "OperatorEscalate", Reason: "OperatorEscalate: TaskDelete: dropped", PendingDelete: true, Version: 4})
	d.details["waiting"] = client.TaskDetail{ID: "waiting", Version: 3, State: "Escalated", EscalationKind: "OperatorEscalate", EscalationReason: "TaskDelete: obsolete",
		PendingDelete: &client.PendingDelete{SupersededBy: "parser", RequestedAt: fixedNow.Add(-15 * time.Minute), Status: "waiting-for-turn"}, Source: "TaskGet"}
	d.details["plain"] = client.TaskDetail{ID: "plain", Version: 2, State: "Escalated", EscalationKind: "OperatorEscalate", EscalationReason: "TaskDelete: not really", Source: "TaskGet"}
	d.details["failed"] = client.TaskDetail{ID: "failed", Version: 4, State: "Escalated", EscalationKind: "OperatorEscalate", EscalationReason: "TaskDelete: dropped",
		PendingDelete: &client.PendingDelete{Reason: "dropped", SupersededBy: "docs", RequestedAt: fixedNow.Add(-30 * time.Minute), Status: "failed"}, Source: "TaskGet"}
	h := newHarness(t, Options{}, d).start()
	line := func(id string) string {
		t.Helper()
		for _, l := range strings.Split(h.screen(), "\n") {
			if strings.Contains(l, " "+id+" ") || strings.HasSuffix(l, " "+id) {
				return l
			}
		}
		t.Fatalf("screen lacks a line for %s:\n%s", id, h.screen())
		return ""
	}
	// Molecules: the Board flag marks docs at its Escalated stage.
	h.contains("× docs")
	if l := line("docs"); !strings.Contains(l, "delete pending: stopped by a TaskDelete that has not finished") {
		t.Fatalf("molecules row %q", l)
	}
	// Graph: the flag marks docs, waiting and failed, and the reason text marks nothing.
	h.press("2")
	h.drain()
	for _, id := range []string{"docs", "waiting", "failed"} {
		if l := line(id); !strings.Contains(l, "deleting") {
			t.Fatalf("graph row %q lacks the deleting marker", l)
		}
	}
	if l := line("plain"); !strings.Contains(l, "Escalated") || strings.Contains(l, "delet") {
		t.Fatalf("graph row %q: a reason starting TaskDelete: is not a pending delete", l)
	}
	// Attention: the TaskGet object says where each delete stands, wrapping
	// long lines in a 140-column terminal so the resolve-uncertain command is
	// never clipped off.
	h.press("3")
	h.drain()
	h.contains("docs  OperatorEscalate  since 2h00m ago  delete pending: retire (replaced): turn in flight is uncertain after restart; run fae resolve-uncertain --action",
		"escalate --id docs to finish retiring",
		"waiting  OperatorEscalate  since: unknown  delete pending: retire, superseded by parser: retires once its turn in flight is collected",
		"failed  OperatorEscalate  since: unknown  delete pending: delete failed, superseded by docs (dropped): the task stays Escalated; retire it or delete it again",
		"plain  OperatorEscalate  since: unknown  TaskDelete: not really")
	if l := line("plain"); strings.Contains(l, "delete pending") {
		t.Fatalf("attention row %q", l)
	}
	// The task view shows the object with its request time.
	h.press("down", "down", "down", "enter")
	h.drain()
	h.contains("Task docs", "Deleting     retire (replaced): turn in flight is uncertain after restart; run fae resolve-uncertain --action escalate --id docs to finish retiring,", "requested 11:00:00")
	// The flag alone, when TaskGet reports no object (an older read), still
	// names resolve-uncertain when the task is parked UncertainDispatch, and
	// otherwise says the read did not report its status.
	detail.PendingDelete = nil
	detail.EscalationReason = "OperatorEscalate"
	d.board.Tasks[4].Reason = "OperatorEscalate"
	d.inbox.Escalated[0].Reason = "OperatorEscalate"
	d.details["docs"] = detail
	h.press("r")
	h.drain()
	h.contains("Deleting     delete pending (the Board flags it; this read did not report its status)")
}

// An edit or delete carries the task version its form was filled from. A
// stale-version refusal is shown with an offer to reload the task; nothing
// is retried on its own, and the reopened dialog updates expected_version
// from the fresh TaskGet while preserving the field values the user modified.
func TestAStaleVersionRefusalOffersToReloadTheTask(t *testing.T) {
	d := fixture()
	stale := errors.New("invalid-params: task parser is at version 6, not the expected 5: read it again (TaskGet) and retry")
	d.fail("TaskUpdate", stale)
	h := newHarness(t, Options{Write: true}, d).start()
	h.press("enter")
	h.drain()
	h.press("e")
	h.typeText(" (renamed)")
	h.press("ctrl+s")
	h.drain()
	if p := d.updates[0]; p.ExpectedVersion != 5 {
		t.Fatalf("TaskUpdate sent expected_version %d, want the version the form was opened with", p.ExpectedVersion)
	}
	h.contains("TaskUpdate refused: task parser changed since you opened the form", "not the expected 5", "Nothing was changed. Reload the task")
	h.press("n")
	if h.m.modal != nil || len(d.called("TaskUpdate")) != 1 {
		t.Fatal("n did not just close the refusal")
	}
	// The task moved on (e.g. an admission deferral or priority change bumped
	// the version to 6); y reads it again and reopens the form on version 6
	// while keeping what the user typed in the modified field.
	p := d.details["parser"]
	p.Version, p.Priority = 6, 35
	d.details["parser"] = p
	h.press("e")
	h.typeText(" (renamed)")
	h.press("ctrl+s")
	h.drain()
	reads := len(d.called("Detail"))
	h.press("y")
	h.drain()
	if len(d.called("TaskUpdate")) != 2 || len(d.called("Detail")) <= reads {
		t.Fatalf("y must reload, not retry: %v", d.called("Detail", "TaskUpdate"))
	}
	h.contains("Edit task parser", "Write the parser (renamed)", "35")
	d.fail("TaskUpdate", nil)
	// Submit without retyping: the preserved title edit is sent against version 6.
	h.press("ctrl+s")
	h.drain()
	if p := d.updates[2]; p.ExpectedVersion != 6 || p.Title == nil || *p.Title != "Write the parser (renamed)" || p.Priority != nil {
		t.Fatalf("TaskUpdate after the reload %+v", p)
	}
	h.contains("TaskUpdate done")
	// A delete carries the version too, and a stale one reopens the delete dialog.
	h.press("esc")
	d.fail("TaskDelete", stale)
	h.press("D")
	h.typeText("gone")
	h.press("ctrl+s", "y")
	h.drain()
	if p := d.deletes[0]; p.ExpectedVersion != 6 {
		t.Fatalf("TaskDelete sent expected_version %d", p.ExpectedVersion)
	}
	h.contains("TaskDelete refused: task parser changed since you opened the form", "open the delete dialog again")
	h.press("y")
	h.drain()
	h.contains("Delete task parser", "gone")
	if len(d.called("TaskDelete")) != 1 {
		t.Fatal("the delete was retried on its own")
	}
}

// The create form makes a Normal-mode task: mode, disposition, task class,
// workflow revision (with or without a workflow id) and target revision are
// sent under the admission names, and mode completeness and commit-external's
// approval phrase are checked before daemon contact.
func TestCreateFormMakesANormalModeTask(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{Write: true}, d).start()
	h.press("c")
	h.contains("A normal-mode task states exactly one disposition")
	h.typeText("deploy")
	h.focusField("content")
	h.typeText("Ship it.")
	h.focusField("mode")
	h.typeText("normal")
	h.press("ctrl+s")
	h.contains("mode normal requires exactly one of dry-run or commit-external")
	h.focusField("disposition")
	h.typeText("both")
	h.press("ctrl+s")
	h.contains("the disposition is dry-run or commit-external")
	h.press("backspace", "backspace", "backspace", "backspace")
	h.typeText("commit-external")
	h.press("ctrl+s")
	h.contains(`commit-external requires "I approve live external effects" in the note`)
	for range len("commit-external") {
		h.press("backspace")
	}
	h.typeText("dry-run")
	h.focusField("mode")
	for range 6 {
		h.press("backspace")
	}
	h.typeText("local")
	h.press("ctrl+s")
	h.contains("dry-run and commit-external require mode normal")
	for range 5 {
		h.press("backspace")
	}
	h.typeText("weird")
	h.press("ctrl+s")
	h.contains("mode must be local or normal")
	for range 5 {
		h.press("backspace")
	}
	// An empty mode with a disposition is normal, as for the CLI.
	// A pinned revision with an empty workflow id means the default workflow.
	h.focusField("revision")
	h.typeText("x")
	h.press("ctrl+s")
	h.contains("the workflow revision is a whole number")
	h.press("backspace")
	h.typeText("3")
	h.focusField("target_revision")
	h.typeText("2")
	h.press("ctrl+s")
	h.contains("a target revision needs the target id")
	h.focusField("target")
	h.typeText("tg")
	h.focusField("task_class")
	h.typeText("heavy")
	if len(d.called(writeMethods...)) != 0 {
		t.Fatal("an invalid create was sent")
	}
	h.press("ctrl+s")
	h.drain()
	if len(d.creates) != 1 {
		t.Fatalf("creates %v, screen:\n%s", d.creates, h.screen())
	}
	p := d.creates[0].TaskFields
	want := client.TaskFields{ID: "deploy", Content: "Ship it.", Priority: 10, DeclaredFiles: []string{}, WorkflowID: "", Revision: 3, TargetConfig: "tg", TargetRevision: 2, TaskClass: "heavy", Mode: "Normal", DryRun: true}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("TaskCreate fields %+v, want %+v", p, want)
	}
	if !strings.Contains(d.messages[0], "create task deploy in normal mode, dry-run") {
		t.Fatalf("message %s", d.messages[0])
	}
	h.contains("TaskCreate done")
	// A commit-external create succeeds once the note carries the approval phrase.
	h.press("esc", "c")
	h.typeText("live-deploy")
	h.focusField("content")
	h.typeText("Ship it live.")
	h.focusField("disposition")
	h.typeText("commit-external")
	h.focusField("note")
	h.typeText("I approve live external effects for this release")
	h.press("ctrl+s")
	h.drain()
	if len(d.creates) != 2 || !d.creates[1].CommitExternal || d.creates[1].DryRun || d.creates[1].Mode != "Normal" {
		t.Fatalf("commit-external create %+v", d.creates)
	}
	if !strings.Contains(d.messages[1], "I approve live external effects") {
		t.Fatalf("commit-external message %s", d.messages[1])
	}
}

func TestAWriteToADaemonWithoutTheVerbSaysSo(t *testing.T) {
	d := fixture()
	d.fail("TaskCreate", errors.New("actor is not permitted to perform action"))
	h := newHarness(t, Options{Write: true}, d).start()
	h.press("c")
	h.typeText("x")
	h.press("tab", "tab")
	h.typeText("Do it.")
	h.press("ctrl+s")
	h.drain()
	h.contains("TaskCreate refused", "this daemon has no TaskCreate: it predates live task editing")
}

func TestMoleculesNavigationOpensTheTaskAndItsTrace(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{}, d).start()
	if h.m.selectedMoleculeTask() != "parser" {
		t.Fatalf("first task row %q", h.m.selectedMoleculeTask())
	}
	h.press("down")
	if h.m.selectedMoleculeTask() != "docs" {
		t.Fatalf("down selected %q", h.m.selectedMoleculeTask())
	}
	h.press("up", "enter")
	if got := h.queued(); !reflect.DeepEqual(got, []string{"detail"}) {
		t.Fatalf("enter queued %v", got)
	}
	h.contains("reading task parser")
	h.drain()
	h.contains("Task parser  Write the parser", "Parse the input format.", "Reject malformed input.", "digest abc123",
		"declared files  src/parser.x, tests/parser.t", "⇒ api", "Trace: 3 stages, 1 edit")
	// The trace in order, the edit between the stages it fell between.
	order := []int{h.index("1  Pending"), h.index("2  spec"), h.index("edited title, priority"), h.index("3  implement")}
	if !slices.IsSorted(order) {
		t.Fatalf("trace out of order: %v\n%s", order, h.screen())
	}
	h.contains("released", "completed d-1", "30m00s…", "message msg-edit")
	// A refresh re-reads the open task.
	reads := len(d.called("Detail"))
	h.send(tickMsg{})
	h.drain()
	if got := len(d.called("Detail")) - reads; got != 1 {
		t.Fatalf("the open task was read %d times after a refresh", got)
	}
	h.press("esc")
	h.contains("Molecule m1")
}

func TestTaskViewSaysWhatAnOlderDaemonCannotReport(t *testing.T) {
	d := fixture()
	d.details["api"] = client.TaskDetail{ID: "api", State: "Pending", Priority: 20, Source: "TaskHistory",
		Missing: []string{"title", "description", "acceptance", "structured fields", "edits"},
		Trace:   []client.TraceEntry{{Stage: "Pending", EnteredAt: fixedNow.Add(-time.Hour)}}}
	h := newHarness(t, Options{}, d).start()
	h.press("2", "right")
	if h.m.graph.sel != "api" {
		t.Fatalf("right selected %q", h.m.graph.sel)
	}
	h.press("enter")
	h.drain()
	h.contains("Task api", "(no title)", "not reported by this daemon (it has no TaskGet)", "this daemon has no TaskGet, so the trace is rebuilt from the transition log")
}

func TestGraphShowsLayersEdgesAndImplementationStates(t *testing.T) {
	h := newHarness(t, Options{}, fixture()).start()
	h.press("2")
	h.contains("layer 0 (5)", "layer 1 (2)", "layer 2 (1)")
	// Every state class is labelled.
	h.contains("✓ lib", "Done", "▶ parser", "implement", "! docs", "Escalated", "○ notes", "ready", "× old", "Cancelled", "◌ api", "withheld")
	// The selected node's edges, with the far end's state; dependents are marked.
	h.contains("prerequisites ↑ none", "dependents    ↓ ◌ api (blocking, withheld by parser: implement)", "↓◌ api")
	if h.index("layer 0") > h.index("layer 1") || h.index("layer 1") > h.index("layer 2") {
		t.Fatal("layers out of order")
	}
	// The list form gives every task its full state and its edges, dead ones marked.
	h.press("v")
	h.contains("── layer 1 · 2 tasks ──", "withheld by old: Cancelled", "⇐old ✗dead", "⇐lib ✓, ⇐parser ▶", "~docs")
	// A dead edge is named on the dependent's side too.
	h.press("down", "down", "down", "down", "down", "down")
	if h.m.graph.sel != "cleanup" {
		t.Fatalf("selected %q", h.m.graph.sel)
	}
	h.contains("prerequisites ↑ × old (blocking, Cancelled) ✗ dead")
	// Hiding finished tasks drops them and their edges from the layout.
	h.press("f")
	h.lacks("✓ lib", "× old")
	h.contains("finished tasks hidden (f)")
}

func TestGraphNavigationFindAndEdgeJumps(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{}, d).start()
	h.press("2")
	if h.m.graph.sel != "lib" {
		t.Fatalf("default selection %q", h.m.graph.sel)
	}
	h.press("down")
	if h.m.graph.sel != "parser" {
		t.Fatalf("down selected %q", h.m.graph.sel)
	}
	h.press("right", "right")
	if h.m.graph.sel != "app" {
		t.Fatalf("right right selected %q", h.m.graph.sel)
	}
	h.press("p")
	if h.m.graph.sel != "api" {
		t.Fatalf("p selected %q", h.m.graph.sel)
	}
	h.press("n")
	if h.m.graph.sel != "app" {
		t.Fatalf("n selected %q", h.m.graph.sel)
	}
	h.press("/")
	h.typeText("clean")
	h.press("enter")
	if h.m.graph.sel != "cleanup" || h.m.modal != nil {
		t.Fatalf("find selected %q", h.m.graph.sel)
	}
	h.press("/")
	h.typeText("nothing")
	h.press("enter")
	h.contains(`no task matches "nothing"`)
	h.drain()
	h.press("enter")
	if got := h.queued(); !reflect.DeepEqual(got, []string{"detail"}) {
		t.Fatalf("enter queued %v", got)
	}
}

func TestGraphLayoutScalesAndScrolls(t *testing.T) {
	d := fixture()
	d.board.Tasks, d.backlog = nil, client.Backlog{}
	for i := range 300 {
		id := fmt.Sprintf("t%03d", i)
		d.board.Tasks = append(d.board.Tasks, client.Task{ID: id, State: "Pending", Priority: 10})
		if i > 0 {
			d.backlog.Edges = append(d.backlog.Edges, client.Edge{From: fmt.Sprintf("t%03d", i/2), To: id, Kind: "blocking"})
		}
	}
	h := newHarness(t, Options{}, d).start()
	h.press("2")
	// t299's chain to t000 is 9 edges long: t299 → t149 → … → t001 → t000.
	if got := len(h.m.graph.layout.Layers); got != 10 {
		t.Fatalf("the tree has %d layers, want 10", got)
	}
	for range 12 {
		h.press("right")
	}
	if layer := h.m.graph.layout.Layer[h.m.graph.sel]; layer != 9 {
		t.Fatalf("selection at layer %d", layer)
	}
	h.press("end")
	h.contains(h.m.graph.sel, "showing layers 5–9 of 10")
	if lines := strings.Count(h.screen(), "\n") + 1; lines != h.m.height {
		t.Fatalf("screen has %d lines, want %d", lines, h.m.height)
	}
	h.press("v", "end")
	h.contains("t299")
}

func TestAttentionListsEscalatedTasksAndAlerts(t *testing.T) {
	d := fixture()
	d.status.Snapshot.Quarantined = map[string]string{"bad": "undecodable checkpoint"}
	d.inbox.Decisions = []client.InboxTask{{Task: "pick", Stage: "choose", Decision: []byte(`{"question":"Ship A or B?"}`)}}
	h := newHarness(t, Options{}, d).start()
	h.press("3")
	h.contains("Escalated (1)", "docs  AttemptCap  since 2h00m ago  attempt limit 3 reached",
		"Quarantined (out of the drive until restart) (1)", "bad  undecodable checkpoint",
		"Waiting for a decision (1)", "pick  at choose  — Ship A or B?",
		"Unacknowledged alerts (1)", "#7 warning route-exhaustion  task docs  5m00s ago  no eligible route for 30m")
	if h.index("Escalated (1)") > h.index("Unacknowledged alerts") {
		t.Fatal("escalated tasks must come first")
	}
	h.press("enter")
	h.drain()
	h.contains("Task docs  Write the docs", "AttemptCap: attempt limit 3 reached", "since 10:00:00")
}

func TestReadOnlySessionSendsNoWrite(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{}, d).start()
	for _, view := range []string{"1", "2", "3"} {
		h.press(view)
		for _, k := range []string{"c", "e", "D", "+", "-", "A"} {
			h.press(k)
			if h.m.modal != nil {
				t.Fatalf("%s opened a dialog in a read-only session", k)
			}
		}
	}
	h.press("2", "enter")
	h.drain()
	h.press("e", "D", "+", "-")
	h.drain()
	if got := d.called(writeMethods...); len(got) != 0 {
		t.Fatalf("a read-only session called %v", got)
	}
	h.contains("read-only: start fae-tui with --write")
}

func TestCreateSendsTheAuthorizingMessageFirst(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{Write: true}, d).start()
	h.press("c")
	h.contains("Create a task", "Create refuses an id that already exists")
	h.typeText("search")
	h.press("tab")
	h.typeText("Search box")
	h.press("tab")
	h.typeText("Add a search box.")
	h.press("tab", "backspace", "backspace")
	h.typeText("25")
	h.focusField("blocked_by")
	h.typeText("lib api")
	h.press("tab")
	h.typeText("app")
	h.press("tab")
	h.typeText("notes")
	h.press("tab")
	h.typeText("asked for in review")
	h.press("ctrl+s")
	if got := h.queued(); !reflect.DeepEqual(got, []string{"write:TaskCreate"}) {
		t.Fatalf("submit queued %v", got)
	}
	if len(d.called(writeMethods...)) != 0 {
		t.Fatal("the write ran on the update loop")
	}
	h.drain()
	if got := d.called(writeMethods...); !reflect.DeepEqual(got, []string{"Message", "TaskCreate"}) {
		t.Fatalf("calls %v, want the message first", got)
	}
	if !strings.Contains(d.messages[0], `"authorize":{"classes":["task"]}`) || !strings.Contains(d.messages[0], "create task search (Search box) with 4 edges") || !strings.Contains(d.messages[0], "asked for in review") {
		t.Fatalf("authorizing message %s", d.messages[0])
	}
	p := d.creates[0]
	want := []client.Edge{{From: "lib", To: "search", Kind: "blocking"}, {From: "api", To: "search", Kind: "blocking"}, {From: "search", To: "app", Kind: "blocking"}, {From: "search", To: "notes", Kind: "relates"}}
	if p.ID != "search" || p.Title != "Search box" || p.Content != "Add a search box." || p.Priority != 25 || p.AuthorizingMessage != "msg-1" || !reflect.DeepEqual(p.Edges, want) {
		t.Fatalf("TaskCreate %+v", p)
	}
	h.contains("TaskCreate done", "created search, state Pending", "authorizing message msg-1")
	if !slices.Contains(h.queued(), "refresh") && len(d.called("Board")) < 2 {
		t.Fatal("a write must refresh the views")
	}
	h.press("enter")
	if h.m.modal != nil {
		t.Fatal("enter did not close the result")
	}
}

func TestCreateAsksBeforeHoldingADispatchedDependent(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{Write: true}, d).start()
	h.press("c")
	h.typeText("schema")
	h.press("tab", "tab")
	h.typeText("Define the schema.")
	h.focusField("blocks")
	h.typeText("parser")
	h.press("ctrl+s")
	h.contains("Dependents already dispatched", "parser already left Pending", "Accept these edges in flight?")
	h.press("y")
	h.drain()
	if p := d.creates[0]; !p.AcceptInFlight || !reflect.DeepEqual(p.Edges, []client.Edge{{From: "schema", To: "parser", Kind: "blocking"}}) {
		t.Fatalf("TaskCreate %+v", p)
	}
	if !strings.Contains(d.messages[0], "accepts its edges to dependents already dispatched") {
		t.Fatalf("message %s", d.messages[0])
	}
}

func TestCreateValidatesBeforeSending(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{Write: true}, d).start()
	h.press("c", "ctrl+s")
	h.contains("the id is required")
	h.typeText("x")
	h.press("ctrl+s")
	h.contains("the description is required")
	h.press("tab", "tab")
	h.typeText("Do it.")
	h.press("tab", "backspace", "backspace")
	h.typeText("5")
	h.press("ctrl+s")
	h.contains("priority must be a whole number of at least 10")
	h.press("esc")
	if h.m.modal != nil || len(d.called(writeMethods...)) != 0 {
		t.Fatal("an invalid or cancelled create was sent")
	}
}

func TestUpdateSendsOnlyChangedFieldsAndSaysWhenTheyApply(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{Write: true}, d).start()
	// From a view, the edit reads the task first, then opens the form.
	h.press("e")
	h.drain()
	h.contains("Edit task parser", "edits apply from its next stage; the turn in flight keeps the text")
	h.lacks("reading task parser")
	h.press("ctrl+s")
	h.contains("nothing changed")
	h.typeText(" v2")
	h.press("tab", "tab")
	h.press("backspace", "backspace")
	h.typeText("40")
	h.press("ctrl+s")
	h.drain()
	if got := d.called(writeMethods...); !reflect.DeepEqual(got, []string{"Message", "TaskUpdate"}) {
		t.Fatalf("calls %v", got)
	}
	p := d.updates[0]
	if p.ID != "parser" || p.Title == nil || *p.Title != "Write the parser v2" || p.Priority == nil || *p.Priority != 40 || p.Content != nil || p.DeclaredFiles != nil || p.AuthorizingMessage != "msg-1" || p.ExpectedVersion != 5 {
		t.Fatalf("TaskUpdate %+v", p)
	}
	if !reflect.DeepEqual(p.Changed(), []string{"priority", "title"}) || !strings.Contains(d.messages[0], "edit task parser: priority, title") {
		t.Fatalf("changed %v, message %s", p.Changed(), d.messages[0])
	}
	h.contains("TaskUpdate done", "the edits apply from its next stage")
}

func TestUpdateSendsAScopeChangeOfATurnInFlight(t *testing.T) {
	// Wave 13: the daemon applies declared files, test filter and test shape
	// from the next dispatch (the turn in flight keeps the values it was
	// dispatched under), so the form sends them like every other field, the
	// result dialog names both text and structured fields, and the task view
	// shows superseded_fields while the turn is in flight.
	d := fixture()
	h := newHarness(t, Options{Write: true}, d).start()
	h.press("enter")
	h.drain()
	h.press("e")
	h.contains("keeps the declared files, test filter and test shape it was dispatched under")
	for range 6 {
		h.press("tab")
	}
	h.typeText("|lexer")
	p := d.details["parser"]
	p.DeclaredFiles = []string{"src/new.x"}
	p.TestFilter = "parser|lexer"
	p.SupersededFields = &client.StructuredFields{DeclaredFiles: []string{"src/parser.x", "tests/parser.t"}, TestFilter: "parser", TestShape: "go-unit"}
	d.details["parser"] = p
	h.press("ctrl+s")
	h.drain()
	if len(d.updates) != 1 || d.updates[0].TestFilter == nil || !strings.HasSuffix(*d.updates[0].TestFilter, "|lexer") {
		t.Fatalf("a scope change of a turn in flight was not sent: %+v", d.updates)
	}
	h.contains("the task is running: the edits apply from its next stage;",
		"the turn in flight keeps its text and dispatched structured fields")
	h.press("esc")
	h.contains("declared files  src/new.x", "test filter     parser|lexer",
		"turn in flight runs under (edits above apply from the next dispatch):",
		"declared files  src/parser.x, tests/parser.t", "test filter     parser", "test shape      go-unit")
}

func TestTaskViewDescribesEveryRecordedChange(t *testing.T) {
	d := fixture()
	p := d.details["parser"]
	p.Edits = append(p.Edits,
		client.Edit{At: fixedNow.Add(-35 * time.Minute), Kind: "backlog_edge_changed", Detail: []byte(`{"from":"parser","to":"api","kind":"blocking"}`)},
		client.Edit{At: fixedNow.Add(-25 * time.Minute), Kind: "backlog_edge_changed", Detail: []byte(`{"from":"x","to":"parser","kind":"relates","removed":true}`)},
		client.Edit{At: fixedNow.Add(-1 * time.Minute), Kind: "task-deleted", Detail: []byte(`{"action":"retired","retirement":"obsolete gone"}`)},
	)
	p.Trace[1].To, p.Trace[1].Outcome = "implement", "completed d-1"
	p.PendingDelete, p.Attempts, p.AttemptLimit, p.Readiness = &client.PendingDelete{Reason: "gone", RequestedAt: fixedNow.Add(-time.Minute), Status: "retiring"}, 1, 3, "ready"
	p.EditsTruncated = true
	d.details["parser"] = p
	h := newHarness(t, Options{}, d).start()
	h.press("enter")
	h.drain()
	h.contains("edge added parser → api (blocking)", "edge removed x → parser (relates)", "deleted: retired obsolete gone",
		"completed d-1 → implement", "Deleting     retire (gone): retiring at the next drive round, requested 11:59:00", "Attempts     1 of 3", "Trace: 3 stages, 4 edits",
		"(older edits not shown: the task has more edits than TaskGet returns)")
	h.lacks("Readiness")
	order := []int{h.index("1  Pending"), h.index("2  spec"), h.index("edited title"), h.index("edge added"), h.index("3  implement"), h.index("edge removed"), h.index("deleted: retired")}
	if !slices.IsSorted(order) {
		t.Fatalf("changes out of order: %v\n%s", order, h.screen())
	}
}

func TestAPendingTaskShowsItsReadiness(t *testing.T) {
	d := fixture()
	api := d.details["api"]
	api.Readiness = "withheld by parser: implement"
	d.details["api"] = api
	h := newHarness(t, Options{}, d).start()
	h.press("2", "right", "enter")
	h.drain()
	h.contains("Readiness    withheld by parser: implement", "⇐ lib", "⇐ parser")
	// On Molecules (where BacklogShow is not polled), when parser finishes on
	// the Board, opening parser or api shows api as ready, never stale
	// "withheld by parser: implement".
	h.press("esc", "1", "enter")
	h.drain()
	for i := range d.board.Tasks {
		if d.board.Tasks[i].ID == "parser" {
			d.board.Tasks[i].State, d.board.Tasks[i].InFlight, d.board.Tasks[i].Version = "Done", false, 9
		}
	}
	p := d.details["parser"]
	p.State, p.InFlight = "Done", false
	d.details["parser"] = p
	h.press("r")
	h.drain()
	h.contains("⇒ api                          blocking  ○ Pending, ready")
	h.lacks("withheld by parser: implement")
	h.m.openDetail("api")
	h.drain()
	h.contains("State        ○ Pending, ready", "⇐ parser                       blocking  ✓ Done")
	h.lacks("State        ◌ withheld by parser")
}

func TestDeleteSaysWhatWillHappenAndShowsARefusal(t *testing.T) {
	d := fixture()
	d.fail("TaskDelete", errors.New("task parser has an unresolved dispatch"))
	h := newHarness(t, Options{Write: true}, d).start()
	h.press("enter")
	h.drain()
	h.press("D")
	h.contains("Delete task parser", "the delete stops it and waits for that turn's result, then retires the task", "Its blocking dependents (api) will be held")
	h.press("ctrl+s")
	h.contains("a delete needs a reason")
	h.typeText("replaced")
	h.press("tab")
	h.typeText("search")
	h.press("ctrl+s")
	h.contains("Delete task parser?", "Its blocking dependents (api) will wait on search instead.")
	h.press("n")
	if h.m.modal != nil || len(d.called(writeMethods...)) != 0 {
		t.Fatal("n did not cancel")
	}
	h.press("D")
	h.typeText("superseded by the new design")
	h.press("ctrl+s", "y")
	h.drain()
	if got := d.called(writeMethods...); !reflect.DeepEqual(got, []string{"Message", "TaskDelete"}) {
		t.Fatalf("calls %v", got)
	}
	h.contains("TaskDelete refused", "task parser has an unresolved dispatch")
	d.fail("TaskDelete", nil)
	h.press("esc", "D")
	h.typeText("gone")
	h.press("tab")
	h.typeText("search")
	h.press("ctrl+s", "y")
	h.drain()
	if p := d.deletes[1]; p.ID != "parser" || p.Reason != "gone" || p.SupersededBy != "search" || p.AuthorizingMessage != "msg-2" || p.ExpectedVersion != 5 {
		t.Fatalf("TaskDelete %+v", p)
	}
	if !strings.Contains(d.messages[1], "delete task parser: gone. It is superseded by search.") {
		t.Fatalf("message %s", d.messages[1])
	}
	h.contains("parser is stopped and will be retired once its turn in flight is collected; its history stays", "held dependents: api")
}

func TestDeleteOfANeverStartedTaskIsAWithdrawal(t *testing.T) {
	h := newHarness(t, Options{Write: true}, fixture()).start()
	h.press("2", "right", "enter")
	h.drain()
	h.press("D")
	h.contains("It never started, so it will be withdrawn.", "Its blocking dependents (app) will be held")
}

func TestTerminalTasksRefuseEditsBeforeAnyCall(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{Write: true}, d).start()
	h.press("2")
	h.drain()
	if h.m.graph.sel != "lib" {
		t.Fatal(h.m.graph.sel)
	}
	reads := len(d.called("Detail"))
	h.press("e")
	h.contains("task lib is Done: a terminal task cannot be changed")
	h.press("D")
	if h.m.modal != nil || len(d.called(writeMethods...)) != 0 || len(d.called("Detail")) != reads || len(h.jobs) != 0 {
		t.Fatal("a terminal task was edited")
	}
	// A task the Board no longer lists is checked once its detail is read.
	d.details["gone"] = client.TaskDetail{ID: "gone", State: "Retired", Source: "TaskGet"}
	h.m.openDetail("gone")
	h.drain()
	h.press("D")
	h.contains("task gone is Retired: a finished task cannot be deleted")
	h.press("e")
	h.contains("task gone is Retired: a terminal task cannot be edited")
	if h.m.modal != nil || len(d.called(writeMethods...)) != 0 {
		t.Fatal("a retired task was edited")
	}
}

func TestEdgeAddAsksBeforeAcceptingADispatchedDependent(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{Write: true}, d).start()
	h.press("+")
	h.contains("Add an edge")
	h.typeText("lib")
	h.press("ctrl+s")
	h.contains("Dependent parser has already been dispatched", "it does not interrupt a turn in flight")
	h.press("y")
	h.drain()
	if got := d.called(writeMethods...); !reflect.DeepEqual(got, []string{"Message", "BacklogEdge"}) {
		t.Fatalf("calls %v", got)
	}
	if p := d.edges[0]; p.Edge != (client.Edge{From: "lib", To: "parser", Kind: "blocking"}) || !p.AcceptInFlight || p.Remove || p.AuthorizingMessage != "msg-1" {
		t.Fatalf("BacklogEdge %+v", p)
	}
	h.contains("added blocking edge lib → parser", `readiness_effect: {"effect":"held"}`)
	// A Pending dependent needs no acceptance.
	h.press("esc", "2", "right", "+")
	h.typeText("lib")
	h.press("tab", "backspace", "backspace", "backspace")
	h.typeText("app")
	h.press("ctrl+s")
	h.drain()
	if p := d.edges[1]; p.Edge != (client.Edge{From: "lib", To: "app", Kind: "blocking"}) || p.AcceptInFlight {
		t.Fatalf("BacklogEdge %+v", p)
	}
}

func TestARefusalForWantOfAcceptanceOffersIt(t *testing.T) {
	d := fixture()
	d.edgeCheck = func(p client.EdgeParams) error {
		if !p.AcceptInFlight {
			return errors.New("dependent already dispatched; --accept-in-flight required")
		}
		return nil
	}
	h := newHarness(t, Options{Write: true}, d).start()
	// api is Pending on the Board, so nothing is asked before sending.
	h.press("2", "right", "+")
	h.typeText("notes")
	h.press("tab", "tab", "backspace", "backspace", "backspace", "backspace", "backspace", "backspace", "backspace", "backspace")
	h.typeText("relates")
	h.press("ctrl+s")
	h.drain()
	h.contains("BacklogEdge needs your acceptance in flight", "dependent already dispatched; --accept-in-flight required", "Send it again")
	h.press("y")
	h.drain()
	if got := d.called(writeMethods...); !reflect.DeepEqual(got, []string{"Message", "BacklogEdge", "Message", "BacklogEdge"}) {
		t.Fatalf("calls %v", got)
	}
	if p := d.edges[0]; p.Edge != (client.Edge{From: "notes", To: "api", Kind: "relates"}) || !p.AcceptInFlight {
		t.Fatalf("BacklogEdge %+v", p)
	}
	h.contains("BacklogEdge done")
}

func TestEdgeRemovePicksOneOfTheTasksEdges(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{Write: true}, d).start()
	h.press("2", "right", "-")
	h.contains("Remove which edge of api?", "lib → api (blocking)", "parser → api (blocking)", "api → app (blocking)")
	h.press("down", "enter")
	h.contains("Remove the blocking edge parser → api.")
	h.press("y")
	h.drain()
	if p := d.edges[0]; p.Edge != (client.Edge{From: "parser", To: "api", Kind: "blocking"}) || !p.Remove {
		t.Fatalf("BacklogEdge %+v", p)
	}
}

func TestARefusedMessageSendsNothing(t *testing.T) {
	d := fixture()
	d.fail("Message", errors.New("only the operator records messages"))
	h := newHarness(t, Options{Write: true}, d).start()
	h.press("3", "down", "A")
	h.drain()
	if got := d.called(writeMethods...); !reflect.DeepEqual(got, []string{"Message"}) {
		t.Fatalf("calls %v", got)
	}
	h.contains("AlertAcknowledge refused", "the authorizing message was refused, so nothing was sent", "only the operator records messages")
	d.fail("Message", nil)
	h.press("esc", "A")
	h.drain()
	if !reflect.DeepEqual(d.acks, []int64{7}) || !strings.Contains(d.messages[0], `"classes":["runtime"]`) {
		t.Fatalf("acks %v, message %v", d.acks, d.messages)
	}
}

func TestUnreachableDaemonIsShownAndRetried(t *testing.T) {
	d := fixture()
	down := client.Unreachable(errors.New("dial unix /x/run/rpc.sock: connect: no such file or directory"))
	for _, m := range []string{"Status", "Board", "BacklogShow", "Topology", "Alerts", "Inbox"} {
		d.fail(m, down)
	}
	h := newHarness(t, Options{Refresh: 3 * time.Second}, d).start()
	h.contains("daemon unreachable", "retrying every 3s", "waiting for the Molecules read")
	h.send(tickMsg{})
	if got := h.queued(); !reflect.DeepEqual(got, []string{"refresh"}) {
		t.Fatalf("a tick while unreachable queued %v", got)
	}
	for _, m := range []string{"Status", "Board", "BacklogShow", "Topology", "Alerts", "Inbox"} {
		d.fail(m, nil)
	}
	h.drain()
	h.lacks("daemon unreachable")
	h.contains("Molecule m1")
	// It goes away again: the last data stays, with its age.
	for _, m := range []string{"Status", "Board", "BacklogShow", "Topology", "Alerts", "Inbox"} {
		d.fail(m, down)
	}
	h.send(tickMsg{})
	h.drain()
	h.contains("daemon unreachable", "showing data from", "Molecule m1")
}

func TestAFailedReadKeepsThePreviousData(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{}, d).start()
	d.fail("Board", errors.New("snapshot-stale: board snapshot exceeds 10 seconds"))
	h.send(tickMsg{})
	h.drain()
	h.contains("read failed", "Board: snapshot-stale", "▶ parser")
	h.press("2")
	h.contains("✓ lib")
}

func TestKeysQuitHelpAndViews(t *testing.T) {
	h := newHarness(t, Options{Write: true}, fixture()).start()
	if cmd := h.send(key("q")); cmd == nil || cmd() != tea.Quit() {
		t.Fatal("q does not quit")
	}
	h.press("?")
	h.contains("Views", "Writes", "States")
	h.press("x")
	h.lacks("This session is read-only")
	h.press("tab")
	if h.m.view != viewGraph {
		t.Fatal("tab")
	}
	h.press("shift+tab", "shift+tab")
	if h.m.view != viewAttention {
		t.Fatal("shift+tab")
	}
	// In a form q is text, and ctrl+c still quits.
	h.press("c")
	if cmd := h.send(key("q")); cmd != nil && cmd() == tea.Quit() {
		t.Fatal("q quit from inside a form")
	}
	if cmd := h.send(key("ctrl+c")); cmd == nil || cmd() != tea.Quit() {
		t.Fatal("ctrl+c does not quit from a form")
	}
}

// After the first refresh, each poll reads Status, Board, Alerts and Inbox
// (for the header and tab counts, including Attention) plus the view's own
// part (Topology on Molecules, BacklogShow on Graph); a view switch refreshes
// that view at once, and a switch during a refresh in flight is followed by
// one more.
func TestPollsReadOnlyWhatTheViewShows(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{}, d).start()
	counts := func() map[string]int {
		out := map[string]int{}
		for _, m := range parts {
			out[m] = len(d.called(m))
		}
		return out
	}
	if got := counts(); !reflect.DeepEqual(got, map[string]int{"Status": 1, "Board": 1, "BacklogShow": 1, "Topology": 1, "Alerts": 1, "Inbox": 1}) {
		t.Fatalf("the first refresh read %v, want everything once", got)
	}
	h.contains("3 Attention (2)")
	// A new alert and uncertain dispatch arriving while on Molecules update
	// the Attention tab count on the next poll without switching views.
	d.alerts = append(d.alerts, client.Alert{ID: 8, Severity: "error", Source: "leaks", Detail: []byte(`{"reason":"defunct"}`), CreatedAt: fixedNow})
	d.inbox.Uncertain = append(d.inbox.Uncertain, client.InboxTask{Task: "api", Stage: "implement", Reason: "UncertainDispatch"})
	h.poll()
	if got := counts(); !reflect.DeepEqual(got, map[string]int{"Status": 2, "Board": 2, "BacklogShow": 1, "Topology": 2, "Alerts": 2, "Inbox": 2}) {
		t.Fatalf("a Molecules poll read %v", got)
	}
	h.contains("3 Attention (4)")
	h.press("2")
	if got := h.queued(); !reflect.DeepEqual(got, []string{"refresh"}) {
		t.Fatalf("a view switch queued %v", got)
	}
	h.drain()
	h.poll()
	if got := counts(); !reflect.DeepEqual(got, map[string]int{"Status": 4, "Board": 4, "BacklogShow": 3, "Topology": 2, "Alerts": 4, "Inbox": 4}) {
		t.Fatalf("the Graph switch and a poll read %v", got)
	}
	h.press("3")
	h.drain()
	if got := counts(); !reflect.DeepEqual(got, map[string]int{"Status": 5, "Board": 5, "BacklogShow": 3, "Topology": 2, "Alerts": 5, "Inbox": 5}) {
		t.Fatalf("the Attention switch read %v", got)
	}
	h.contains("Escalated (1)", "Uncertain dispatch (resolve-uncertain) (1)", "Unacknowledged alerts (2)")
	// A switch while a refresh is out: the Molecules read follows it.
	h.send(tickMsg{})
	h.press("1")
	if got := h.queued(); !reflect.DeepEqual(got, []string{"refresh"}) {
		t.Fatalf("a switch during a refresh queued %v", got)
	}
	h.drain()
	if got := counts(); got["Topology"] != 3 || got["Alerts"] != 7 {
		t.Fatalf("after the switch in flight %v", got)
	}
	h.contains("Molecule m1")
}

// Polling backs off while nothing changes, never polls faster than the
// daemon's own snapshot age, and comes back to --refresh on a change, a key
// or a write.
func TestPollingBacksOffWhenNothingChanges(t *testing.T) {
	d := fixture()
	h := newHarness(t, Options{Refresh: 2 * time.Second}, d).start()
	delays := []time.Duration{}
	for range backoffAfter + 4 {
		h.poll()
		delays = append(delays, h.lastDelay())
	}
	want := []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 16 * time.Second}
	if !reflect.DeepEqual(delays, want) {
		t.Fatalf("poll intervals %v, want %v", delays, want)
	}
	h.contains("idle: polling every 16.0s")
	// A change brings the interval back.
	d.board.Tasks = append(d.board.Tasks, client.Task{ID: "fresh", State: "Pending", Priority: 10})
	h.poll()
	h.poll()
	if got := h.lastDelay(); got != 2*time.Second {
		t.Fatalf("after a change the interval is %v", got)
	}
	h.lacks("idle: polling")
	// So does r, and a refresh that failed.
	for range backoffAfter + 1 {
		h.poll()
	}
	if got := h.lastDelay(); got != 8*time.Second {
		t.Fatalf("backed off to %v", got)
	}
	h.press("r")
	h.drain()
	h.poll()
	if got := h.lastDelay(); got != 2*time.Second {
		t.Fatalf("after r the interval is %v", got)
	}
	// The daemon's snapshot age bounds it from below.
	d.board.SnapshotAge = 5 * time.Second
	h.poll()
	h.poll()
	if got := h.lastDelay(); got != 5*time.Second {
		t.Fatalf("with a 5s snapshot age the interval is %v", got)
	}
}

// Older edits TaskGet dropped are named in the trace.
func TestTaskViewSaysWhenOlderEditsAreNotShown(t *testing.T) {
	d := fixture()
	p := d.details["parser"]
	p.EditsTruncated = true
	d.details["parser"] = p
	h := newHarness(t, Options{}, d).start()
	h.press("enter")
	h.drain()
	h.contains("Trace: 3 stages, 1 edit", "(older edits not shown: the task has more edits than TaskGet returns)")
	p.EditsTruncated = false
	d.details["parser"] = p
	h.press("r")
	h.drain()
	h.lacks("older edits not shown")
}

func TestRetiredToggleRefreshesWithRetiredTasks(t *testing.T) {
	h := newHarness(t, Options{}, fixture()).start()
	h.press("R")
	if !h.m.includeRetired || !reflect.DeepEqual(h.queued(), []string{"refresh"}) {
		t.Fatalf("R: %v %v", h.m.includeRetired, h.queued())
	}
	h.drain()
	h.contains("showing retired tasks")
}

func TestEveryScreenFitsTheTerminal(t *testing.T) {
	for _, size := range [][2]int{{20, 6}, {30, 11}, {39, 12}, {40, 12}, {48, 14}, {80, 24}, {200, 60}} {
		d := fixture()
		h := newHarness(t, Options{Write: true}, d).start()
		h.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		small := size[0] < minWidth || size[1] < minHeight
		check := func(what string) {
			t.Helper()
			lines := strings.Split(h.m.View().Content, "\n")
			if len(lines) != size[1] {
				t.Fatalf("%dx%d %s: %d lines", size[0], size[1], what, len(lines))
			}
			for i, line := range lines {
				if w := lipgloss.Width(line); w > size[0] {
					t.Fatalf("%dx%d %s: line %d is %d cells wide: %q", size[0], size[1], what, i, w, ansi.Strip(line))
				}
			}
			if notice := strings.Contains(h.screen(), "terminal too small"); notice != small {
				t.Fatalf("%dx%d %s: too-small notice %v", size[0], size[1], what, notice)
			}
		}
		for _, step := range []struct {
			what string
			keys []string
		}{
			{"molecules", []string{"1"}}, {"compact molecules", []string{"z"}}, {"graph", []string{"2"}}, {"graph list", []string{"v"}},
			{"attention", []string{"3"}}, {"help", []string{"?"}}, {"task", []string{"x", "1", "enter"}},
			{"create form", []string{"esc", "c"}}, {"create form, last field", []string{"shift+tab"}},
			{"edit form", []string{"esc", "enter", "e"}}, {"delete form", []string{"esc", "D"}},
			{"edge choice", []string{"esc", "-"}},
		} {
			h.press(step.keys...)
			h.drain()
			check(step.what)
		}
	}
}

func TestMoleculesViewNamesAMoleculeWhoseRevisionCannotRun(t *testing.T) {
	// Wave 13: Topology renders a molecule whose revision this binary cannot
	// run with an error instead of failing the page (FAE-WF-093); the view
	// shows the error and still lists the molecule's tasks.
	d := fixture()
	d.topology.Molecules[0].Error = "revision cannot be run by this binary (FAE-WF-093): brief key must be default"
	d.topology.Molecules[0].RoleOrder = nil
	h := newHarness(t, Options{}, d).start()
	h.contains("Molecule m1", "stage graph unavailable: revision cannot be run by this binary (FAE-WF-093)", "▶ parser")
}

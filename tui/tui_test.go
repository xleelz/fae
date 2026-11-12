package tui

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/tui/client"
)

type callerFunc func(ctx context.Context, method string, params any) (json.RawMessage, error)

func (f callerFunc) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return f(ctx, method, params)
}

func TestTopologyRequestsIncludeRetiredWhenShowRetiredEnabled(t *testing.T) {
	var (
		mu              sync.Mutex
		topologyRetired []bool
	)
	rc := callerFunc(func(_ context.Context, method string, params any) (json.RawMessage, error) {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		switch method {
		case "Topology":
			inc, _ := m["include_retired"].(bool)
			mu.Lock()
			topologyRetired = append(topologyRetired, inc)
			mu.Unlock()
			tasks := []string{"task-live"}
			if inc {
				tasks = append(tasks, "task-retired")
			}
			return json.Marshal(map[string]any{
				"molecules": []map[string]any{{
					"id":          "mol-1",
					"workflow_id": "wf",
					"state":       "Running",
					"stages": []map[string]any{{
						"id":    "code",
						"role":  "implement",
						"tasks": tasks,
					}},
				}},
				"snapshot_age_ms": 100,
			})
		case "Status":
			return json.Marshal(map[string]any{
				"instance": "default",
				"pid":      4242,
				"adapter":  "scripted",
				"snapshot": map[string]any{"counts": map[string]int{"Running": 1}, "total": 1},
			})
		case "Board":
			rows := []map[string]any{{"id": "task-live", "state": "code", "molecule_id": "mol-1", "workflow_id": "wf"}}
			if inc, _ := m["include_retired"].(bool); inc {
				rows = append(rows, map[string]any{"id": "task-retired", "state": "Cancelled", "molecule_id": "mol-1", "workflow_id": "wf", "retired": true})
			}
			return json.Marshal(map[string]any{"tasks": rows, "snapshot_age_ms": 100})
		case "BacklogShow":
			return json.Marshal(map[string]any{"items": []any{}, "edges": []any{}})
		case "Alerts":
			return json.Marshal(map[string]any{"alerts": []any{}})
		case "Inbox":
			return json.Marshal(map[string]any{"items": []any{}})
		default:
			return json.Marshal(map[string]any{})
		}
	})

	c := client.New(rc)
	h := harnessFor(t, Options{}, c).start()

	mu.Lock()
	if len(topologyRetired) != 1 || topologyRetired[0] {
		t.Fatalf("initial Topology include_retired calls = %v, want [false]", topologyRetired)
	}
	mu.Unlock()
	h.lacks("task-retired")

	// Switch to Molecules view and press R to toggle showRetired on.
	h.press("1", "R")
	h.drain()

	mu.Lock()
	if len(topologyRetired) < 2 || !topologyRetired[len(topologyRetired)-1] {
		t.Fatalf("Topology include_retired calls after pressing R = %v, want last call true", topologyRetired)
	}
	mu.Unlock()
	h.contains("showing retired tasks", "task-retired")
}

func TestDetailStateOfUsesReadinessWhenIncomingEdgesEmpty(t *testing.T) {
	d := fixture()
	// Simulate a stale BacklogShow snapshot that still marks task-b as withheld
	// by task-a before the last incoming edge was removed while in Molecules view.
	d.backlog.Withheld = map[string][]string{
		"task-b": {"task-a: code"},
	}
	h := newHarness(t, Options{}, d).start()

	// Without EdgesIn, if TaskGet reports Readiness == "ready", detailStateOf must
	// return ready rather than falling back to the stale snap.backlog.Withheld entry.
	got := h.m.detailStateOf(client.TaskDetail{
		Source:    "TaskGet",
		ID:        "task-b",
		State:     "Pending",
		Readiness: "ready",
		EdgesIn:   nil,
	})
	if got.Short != "ready" || got.Long != "Pending, ready" || got.Class != classReady {
		t.Fatalf("detailStateOf with Readiness=ready and empty EdgesIn = %+v, want {Short:ready Long:Pending, ready Class:classReady}", got)
	}

	// Also verify when Source is omitted in unit-constructed TaskDetail structs.
	gotImplicit := h.m.detailStateOf(client.TaskDetail{
		ID:        "task-b",
		State:     "Pending",
		Readiness: "ready",
		EdgesIn:   nil,
	})
	if gotImplicit.Short != "ready" || gotImplicit.Long != "Pending, ready" || gotImplicit.Class != classReady {
		t.Fatalf("detailStateOf with implicit Source and Readiness=ready = %+v, want ready", gotImplicit)
	}

	// Also verify when Readiness == "" and EdgesIn is empty (from TaskGet or
	// without "edges" in Missing), detailStateOf returns ready rather than
	// falling back to the stale snap.backlog.Withheld prerequisite.
	gotEmptyReadiness := h.m.detailStateOf(client.TaskDetail{
		Source:    "TaskGet",
		ID:        "task-b",
		State:     "Pending",
		Readiness: "",
		EdgesIn:   nil,
	})
	if gotEmptyReadiness.Short != "ready" || gotEmptyReadiness.Long != "Pending, ready" || gotEmptyReadiness.Class != classReady {
		t.Fatalf("detailStateOf with Readiness==\"\" and empty EdgesIn = %+v, want ready", gotEmptyReadiness)
	}
}

func TestSubWorkflowHierarchyRenderingAndNavigation(t *testing.T) {
	d := fixture()
	parentChild := &client.ChildRef{Stage: "consult", Visit: 1, Task: "parser.consult.1", WorkflowID: "child", Revision: 2}
	childParent := &client.ParentRef{Task: "parser", Stage: "consult", Visit: 1}

	for i := range d.board.Tasks {
		if d.board.Tasks[i].ID == "parser" {
			d.board.Tasks[i].State = "consult"
			d.board.Tasks[i].InFlight = false
			d.board.Tasks[i].Reason = "SubWorkflowPending"
			d.board.Tasks[i].Child = parentChild
		}
	}
	d.board.Tasks = append(d.board.Tasks, client.Task{
		ID: "parser.consult.1", State: "draft", Priority: 20, InFlight: true,
		WorkflowID: "child", WorkflowRevision: 2, MoleculeID: "m-child", Version: 1,
		Parent: childParent,
	})

	d.topology.Molecules[0].Stages = []client.Stage{
		{Stage: "Pending", NormalPath: true, Tasks: []string{}},
		{Stage: "spec", Primitive: "AgentTurn", Roles: []string{"spec"}, NormalPath: true, Tasks: []string{}},
		{Stage: "consult", Primitive: "SubWorkflow", Workflow: &client.WorkflowRef{ID: "child", Revision: 2}, NormalPath: true, Tasks: []string{"parser"}},
		{Stage: "review", Primitive: "Review", Roles: []string{"reviewer"}, NormalPath: true, Tasks: []string{}},
		{Stage: "Done", NormalPath: true, Tasks: []string{}},
	}
	d.topology.Molecules = append(d.topology.Molecules, client.Molecule{
		ID: "m-child", WorkflowID: "child", Revision: 2, CreatedAt: fixedNow.Add(-10 * time.Minute),
		RoleOrder: []string{"author"},
		Stages: []client.Stage{
			{Stage: "Pending", NormalPath: true, Tasks: []string{}},
			{Stage: "draft", Primitive: "AgentTurn", Roles: []string{"author"}, NormalPath: true, Tasks: []string{"parser.consult.1"}},
			{Stage: "Done", NormalPath: true, Tasks: []string{}},
		},
	})

	pDetail := d.details["parser"]
	pDetail.State = "consult"
	pDetail.Stage = "consult"
	pDetail.Parked = true
	pDetail.InFlight = false
	pDetail.Reason = "SubWorkflowPending"
	pDetail.Child = parentChild
	d.details["parser"] = pDetail

	d.details["parser.consult.1"] = client.TaskDetail{
		ID: "parser.consult.1", Version: 1, Title: "Consult child", Content: `{"parent_task":"parser"}`,
		Priority: 20, State: "draft", Stage: "draft", InFlight: true,
		Workflow: "child", WorkflowRevision: 2, Molecule: "m-child",
		Parent: childParent,
		Source: "TaskGet",
	}

	h := newHarness(t, Options{}, d).start()

	// Molecules sorts by workflow_id: "child" (m-child) is first, "default" (m1) is second.
	// On m-child, selected task is parser.consult.1, showing parent link.
	h.contains("Molecule m-child", "parser.consult.1", "parent: parser (consult#1)")

	// Pressing c on a child with no active sub-workflow child reports "No active child task".
	h.press("c")
	h.contains("No active child task")

	// Pressing p on parser.consult.1 jumps to parent task parser in molecule m1
	// and triggers Detail + Topology reads.
	detailReads := len(d.called("Detail"))
	topoReads := len(d.called("Topology"))
	h.press("p")
	h.drain()
	if len(d.called("Detail")) <= detailReads || len(d.called("Topology")) <= topoReads {
		t.Fatalf("expected Detail and Topology reads on jump to parent, got Detail=%d Topology=%d", len(d.called("Detail")), len(d.called("Topology")))
	}
	h.contains("Jumped to parent task parser", "Molecule m1", "→ child@r2", "child: parser.consult.1 (child@r2)")

	// Pressing p on parent task parser (which has no parent) reports "No parent task".
	h.press("p")
	h.contains("No parent task")

	// Pressing c on parent task parser jumps to child task parser.consult.1 in molecule m-child.
	h.press("c")
	h.drain()
	h.contains("Jumped to child task parser.consult.1", "Molecule m-child", "parent: parser (consult#1)")

	// Open Task Detail for child task parser.consult.1 and verify Hierarchy section.
	h.press("enter")
	h.drain()
	h.contains("Task parser.consult.1", "Hierarchy", "Parent: parser via stage consult (visit 1) [press p to jump]")

	// Press p in Task Detail view to jump to parent task parser's Detail view.
	h.press("p")
	h.drain()
	h.contains("Jumped to parent task parser", "Task parser", "Hierarchy", "Child: parser.consult.1 on child@r2 at stage consult (visit 1) [press c to jump]")

	// Press c in Task Detail view to jump back to child task parser.consult.1's Detail view.
	h.press("c")
	h.drain()
	h.contains("Jumped to child task parser.consult.1", "Task parser.consult.1", "Parent: parser via stage consult (visit 1) [press p to jump]")

	// Switch to Graph view, select parser, and verify SubWorkflow target and child info are shown.
	h.press("esc", "2")
	h.drain()
	h.press("/")
	h.typeText("parser")
	h.press("enter")
	if h.m.graph.sel != "parser" {
		t.Fatalf("graph selection = %q, want parser", h.m.graph.sel)
	}
	h.contains("→ child@r2", "child: parser.consult.1 (child@r2)")

	// Press c in Graph view to jump to child task parser.consult.1, then p to jump back to parser.
	h.press("c")
	h.drain()
	if h.m.graph.sel != "parser.consult.1" {
		t.Fatalf("after c in Graph view, selection = %q, want parser.consult.1", h.m.graph.sel)
	}
	h.contains("Jumped to child task parser.consult.1", "parent: parser (consult#1)")

	h.press("p")
	h.drain()
	if h.m.graph.sel != "parser" {
		t.Fatalf("after p in Graph view, selection = %q, want parser", h.m.graph.sel)
	}
	h.contains("Jumped to parent task parser")
}

func TestMultiChildFanOutRenderingAndJumpCycling(t *testing.T) {
	d := fixture()
	fanOutChild := &client.ChildRef{
		Stage:      "consult",
		Visit:      1,
		Task:       "parser.consult.1.0",
		WorkflowID: "child",
		Revision:   2,
		Children: []client.ChildRef{
			{Stage: "consult", Visit: 1, Task: "parser.consult.1.0", WorkflowID: "child", Revision: 2},
			{Stage: "consult", Visit: 1, Task: "parser.consult.1.1", WorkflowID: "child", Revision: 2},
			{Stage: "consult", Visit: 1, Task: "parser.consult.1.2", WorkflowID: "child", Revision: 2},
		},
	}
	childParent := &client.ParentRef{Task: "parser", Stage: "consult", Visit: 1}

	for i := range d.board.Tasks {
		if d.board.Tasks[i].ID == "parser" {
			d.board.Tasks[i].State = "consult"
			d.board.Tasks[i].InFlight = false
			d.board.Tasks[i].Reason = "SubWorkflowPending"
			d.board.Tasks[i].Child = fanOutChild
		}
	}
	d.board.Tasks = append(d.board.Tasks,
		client.Task{
			ID: "parser.consult.1.0", State: "draft", Priority: 20, InFlight: true,
			WorkflowID: "child", WorkflowRevision: 2, MoleculeID: "m-child-0", Version: 1,
			Parent: childParent,
		},
		client.Task{
			ID: "parser.consult.1.1", State: "Done", Priority: 20, InFlight: false,
			WorkflowID: "child", WorkflowRevision: 2, MoleculeID: "m-child-1", Version: 1,
			Parent: childParent,
		},
		client.Task{
			ID: "parser.consult.1.2", State: "Pending", Priority: 20, InFlight: false,
			WorkflowID: "child", WorkflowRevision: 2, MoleculeID: "m-child-2", Version: 1,
			Parent: childParent,
		},
	)

	d.topology.Molecules[0].Stages = []client.Stage{
		{Stage: "Pending", NormalPath: true, Tasks: []string{}},
		{Stage: "spec", Primitive: "AgentTurn", Roles: []string{"spec"}, NormalPath: true, Tasks: []string{}},
		{Stage: "consult", Primitive: "SubWorkflow", Workflow: &client.WorkflowRef{ID: "child", Revision: 2}, NormalPath: true, Tasks: []string{"parser"}},
		{Stage: "review", Primitive: "Review", Roles: []string{"reviewer"}, NormalPath: true, Tasks: []string{}},
		{Stage: "Done", NormalPath: true, Tasks: []string{}},
	}
	for idx, childID := range []string{"parser.consult.1.0", "parser.consult.1.1", "parser.consult.1.2"} {
		molID := "m-child-" + string(rune('0'+idx))
		d.topology.Molecules = append(d.topology.Molecules, client.Molecule{
			ID: molID, WorkflowID: "child", Revision: 2, CreatedAt: fixedNow.Add(-10 * time.Minute),
			RoleOrder: []string{"author"},
			Stages: []client.Stage{
				{Stage: "draft", Primitive: "AgentTurn", Roles: []string{"author"}, NormalPath: true, Tasks: []string{childID}},
			},
		})
		d.details[childID] = client.TaskDetail{
			ID: childID, Version: 1, Title: "Fan-out child " + childID, Content: `{"parent_task":"parser"}`,
			Priority: 20, State: d.board.Tasks[len(d.board.Tasks)-3+idx].State, Stage: "draft",
			Workflow: "child", WorkflowRevision: 2, Molecule: molID,
			Parent: childParent,
			Source: "TaskGet",
		}
	}

	pDetail := d.details["parser"]
	pDetail.State = "consult"
	pDetail.Stage = "consult"
	pDetail.Parked = true
	pDetail.InFlight = false
	pDetail.Reason = "SubWorkflowPending"
	pDetail.Child = fanOutChild
	d.details["parser"] = pDetail

	h := newHarness(t, Options{}, d).start()

	// Jump from child 0 to parent parser in Molecules view.
	h.press("p")
	h.drain()
	h.contains(
		"Jumped to parent task parser",
		"Molecule m1",
		"children (3): parser.consult.1.0, parser.consult.1.1, parser.consult.1.2 (child@r2)",
	)

	// Open Task Detail for parser and verify each fan-out child shows its Board state.
	h.press("enter")
	h.drain()
	h.contains(
		"Task parser",
		"Hierarchy",
		"Child: parser.consult.1.0 on child@r2 at stage consult (visit 1) [press c to jump]",
		"[0] parser.consult.1.0 (child@r2) — draft",
		"[1] parser.consult.1.1 (child@r2) — Done",
		"[2] parser.consult.1.2 (child@r2) — Pending",
	)

	// Switch to Graph view (columns and list) and verify multi-child fan-out rendering.
	h.press("esc", "2")
	h.drain()
	h.press("/")
	h.typeText("parser")
	h.press("enter")
	// "/" find might match parser.consult.1.0 first if sorted higher, so select parser directly.
	h.m.selectTask("parser")
	h.contains("children (3): parser.consult.1.0, parser.consult.1.1, parser.consult.1.2 (child@r2)")
	h.press("v")
	h.contains("children (3): parser.consult.1.0, parser.consult.1.1, parser.consult.1.2 (child@r2)")

	// Press c repeatedly on parent parser to cycle through children 0..2 and wrap back to 0.
	h.press("c")
	h.drain()
	if h.m.graph.sel != "parser.consult.1.0" {
		t.Fatalf("1st c selected %q, want parser.consult.1.0", h.m.graph.sel)
	}
	h.contains("Jumped to child task parser.consult.1.0 (1/3)")

	// Re-select parent parser and press c again -> child 1 (2/3).
	h.m.selectTask("parser")
	h.press("c")
	h.drain()
	if h.m.graph.sel != "parser.consult.1.1" {
		t.Fatalf("2nd c selected %q, want parser.consult.1.1", h.m.graph.sel)
	}
	h.contains("Jumped to child task parser.consult.1.1 (2/3)")

	// Pressing c consecutively without even leaving the child task also cycles to child 2 (3/3), then wraps to child 0 (1/3).
	h.press("c")
	h.drain()
	if h.m.graph.sel != "parser.consult.1.2" {
		t.Fatalf("3rd c selected %q, want parser.consult.1.2", h.m.graph.sel)
	}
	h.contains("Jumped to child task parser.consult.1.2 (3/3)")

	h.press("p")
	h.drain()
	h.press("c")
	h.drain()
	if h.m.graph.sel != "parser.consult.1.0" {
		t.Fatalf("4th c selected %q, want parser.consult.1.0", h.m.graph.sel)
	}
	h.contains("Jumped to child task parser.consult.1.0 (1/3)")
}

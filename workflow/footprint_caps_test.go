package workflow

import "testing"

func TestTreeFootprintRoleCapsAndSharedChildBudget(t *testing.T) {
	d := footprintDoc()
	d.SlotScope = "tree"
	d.Roles = map[string]Role{"author": {Cap: 8, CapScope: "tree"}}
	d.Start = "fan"
	d.Stages["fan"] = Stage{Primitive: FanOut, Parallel: true, Join: "join", BranchesFrom: &BranchesFromSpec{Max: 32}, BranchTemplate: &FanBranch{ID: "template", Start: "agent"}}
	d.Stages["join"] = Stage{Primitive: FanIn}
	d.Edges = []Edge{{From: "fan", To: "agent"}, {From: "agent", To: "join"}, {From: "join", To: "Done"}}
	f, err := DeriveFootprint(d, nil, 0)
	if err != nil || f.DerivedSessions != 8 || f.GraphSessions == nil || *f.GraphSessions != 32 || f.RoleCapBound == nil || *f.RoleCapBound != 8 {
		t.Fatal(f, err)
	}
	d.SessionCount = 8
	limited, limitErr := DeriveFootprint(d, nil, 10)
	if limitErr != nil || limited.DerivedSessions != 8 || limited.EffectiveSessions != 10 {
		t.Fatal("admission cap changed content floor", limited, limitErr)
	}
	d.SessionCount = 0
	role := d.Roles["author"]
	role.Cap = 64
	d.Roles["author"] = role
	f, err = DeriveFootprint(d, nil, 0)
	if err != nil || f.DerivedSessions != 32 {
		t.Fatal(f, err)
	}
	d.Recursion = &Recursion{MaxDepth: 8, MaxDescendants: 32, MaxChildrenPerVisit: 8}
	role.Cap = 2
	role.CapScope = "task"
	d.Roles["author"] = role
	d.Stages["child"] = Stage{Primitive: SubWorkflow, Workflow: &WorkflowRef{ID: "self"}}
	d.Edges = []Edge{{From: "fan", To: "agent"}, {From: "agent", To: "join"}, {From: "join", To: "child"}, {From: "child", To: "Done"}}
	f, err = DeriveFootprint(d, nil, 0)
	if err != nil || f.RoleCapBound == nil || *f.RoleCapBound != 66 || f.DerivedSessions != 66 {
		t.Fatal(f, err)
	}
	// A fan visit's child budget is shared across its eight branch stages.
	d.Recursion = &Recursion{MaxDepth: 8, MaxDescendants: 32, MaxChildrenPerVisit: 8}
	d.Roles = nil
	d.Stages["agent"] = Stage{Primitive: SubWorkflow, Workflow: &WorkflowRef{ID: "leaf", Revision: 1}, InputMap: []ArtifactMap{{Input: "profile"}}}
	delete(d.Stages, "child")
	d.Edges = []Edge{{From: "fan", To: "agent"}, {From: "agent", To: "join"}, {From: "join", To: "Done"}}
	fan := d.Stages["fan"]
	fan.BranchesFrom.Max = 8
	d.Stages["fan"] = fan
	leaf := footprintDoc()
	leaf.ID = "leaf"
	f, err = DeriveFootprint(d, func(WorkflowRef) (Document, error) { return leaf, nil }, 0)
	if err != nil || f.DerivedSessions != 8 {
		t.Fatal(f, err)
	}
	d.Recursion = nil
	fan = d.Stages["fan"]
	fan.BranchesFrom.Max = 32
	d.Stages["fan"] = fan
	f, err = DeriveFootprint(d, func(WorkflowRef) (Document, error) { return leaf, nil }, 0)
	if err != nil || f.DerivedSessions != 32 {
		t.Fatal("legacy parallel children were underpriced", f, err)
	}
	d.Recursion = &Recursion{MaxDepth: 8, MaxDescendants: 32, MaxChildrenPerVisit: 8}
	fan.BranchesFrom.Max = 8
	d.Stages["fan"] = fan
	call := d.Stages["agent"]
	call.ItemsFrom = &Projection{Source: "input:items"}
	d.Stages["agent"] = call
	f, err = DeriveFootprint(d, func(WorkflowRef) (Document, error) { return leaf, nil }, 0)
	if err != nil || f.DerivedSessions != 8 {
		t.Fatal("shared multi-child budget multiplied per branch", f, err)
	}
}

func TestMutualTreeFootprintUsesRecursionFamilyBound(t *testing.T) {
	a := footprintDoc()
	a.ID = "a"
	a.SlotScope = "tree"
	a.Recursion = &Recursion{MaxDepth: 8, MaxDescendants: 5, MaxChildrenPerVisit: 1}
	a.Roles = map[string]Role{"author": {Cap: 1, CapScope: "task"}}
	a.Stages["child"] = Stage{Primitive: SubWorkflow, Workflow: &WorkflowRef{ID: "b", Revision: 1}}
	a.Edges = []Edge{{From: "agent", To: "child"}, {From: "child", To: "Done"}}
	b := footprintDoc()
	b.ID = "b"
	b.SlotScope = "tree"
	b.Recursion = &Recursion{MaxDepth: 8, MaxDescendants: 5, MaxChildrenPerVisit: 1}
	b.Roles = map[string]Role{"author": {Cap: 1, CapScope: "task"}}
	b.Stages["child"] = Stage{Primitive: SubWorkflow, Workflow: &WorkflowRef{ID: "a", Revision: 1}}
	b.Edges = []Edge{{From: "agent", To: "child"}, {From: "child", To: "Done"}}
	resolver := func(ref WorkflowRef) (Document, error) {
		if ref.ID == "a" {
			return a, nil
		}
		return b, nil
	}
	f, err := DeriveFootprint(a, resolver, 0)
	if err != nil || f.EffectiveSessions < 1 || f.EffectiveSessions > 12 {
		t.Fatal("bounded mutual recursion refused or multiplied without bound", f, err)
	}
	a.Stages["child"] = Stage{Primitive: SubWorkflow, Workflow: &WorkflowRef{ID: "self"}}
	a.Recursion.MaxDescendants = 0
	f, err = DeriveFootprint(a, nil, 0)
	if err != nil || f.DerivedSessions != 1 {
		t.Fatal("zero descendant budget should refuse child at its visit, not authoring", f, err)
	}
}

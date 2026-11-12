package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func footprintDoc() Document {
	return Document{ID: "footprint", Start: "agent", Stages: map[string]Stage{"agent": {Primitive: AgentTurn, Role: "author"}, "gate": {Primitive: Gate}}, Edges: []Edge{{From: "agent", To: "gate"}, {From: "gate", To: "Done"}}}
}
func TestFootprintDerivedContentAndDeclaredFloor(t *testing.T) {
	d := footprintDoc()
	check := func(want int) {
		t.Helper()
		f, e := DeriveFootprint(d, nil, 0)
		if e != nil || f.DerivedSessions != want || f.EffectiveSessions != want {
			t.Fatal(f, e, "want", want)
		}
	}
	check(1)
	d.Stages["review"] = Stage{Primitive: Review, Reviewers: []string{"correctness", "security", "quality", "architecture"}, Count: 1}
	d.Edges = []Edge{{From: "agent", To: "review"}, {From: "review", To: "Done"}}
	check(5)
	d.SessionCount = 2
	if _, err := DeriveFootprint(d, nil, 0); err == nil || !strings.Contains(err.Error(), "session_count") {
		t.Fatal("understated footprint accepted", err)
	}
	d.SessionCount = 0
	d.SlotScope = "tree"
	d.MaxSessions = 6
	f, err := DeriveFootprint(d, nil, 0)
	if err != nil || f.DerivedSessions != 4 || f.EffectiveSessions != 6 {
		t.Fatal(f, err)
	}
	d = footprintDoc()
	d.SlotScope = "tree"
	d.Start = "fan"
	d.Stages["fan"] = Stage{Primitive: FanOut, Parallel: true, Join: "join", BranchesFrom: &BranchesFromSpec{Max: 8}, BranchTemplate: &FanBranch{ID: "template", Start: "agent"}}
	d.Stages["join"] = Stage{Primitive: FanIn}
	d.Edges = []Edge{{From: "fan", To: "agent"}, {From: "agent", To: "join"}, {From: "join", To: "Done"}}
	check(8)
	d = footprintDoc()
	d.SlotScope = "tree"
	d.Stages["child"] = Stage{Primitive: SubWorkflow, Workflow: &WorkflowRef{ID: "self"}}
	d.Edges = []Edge{{From: "agent", To: "child"}, {From: "child", To: "Done"}}
	if _, err := DeriveFootprint(d, nil, 0); err == nil {
		t.Fatal("unbounded self recursion accepted")
	}
	d.MaxSessions = 3
	f, err = DeriveFootprint(d, nil, 0)
	if err != nil || f.EffectiveSessions != 3 {
		t.Fatal(f, err)
	}
}
func TestMoleculeFootprintDistinctRolePerPathAndPinnedChild(t *testing.T) {
	d := footprintDoc()
	d.Stages["again"] = Stage{Primitive: AgentTurn, Role: "author"}
	d.Edges = []Edge{{From: "agent", To: "again"}, {From: "again", To: "Done"}}
	f, e := DeriveFootprint(d, nil, 0)
	if e != nil || f.DerivedSessions != 1 {
		t.Fatal(f, e)
	}
	d.Stages["child"] = Stage{Primitive: SubWorkflow, Workflow: &WorkflowRef{ID: "child", Revision: 2}}
	d.Edges = []Edge{{From: "agent", To: "child"}, {From: "child", To: "Done"}}
	f, e = DeriveFootprint(d, func(ref WorkflowRef) (Document, error) {
		if ref.Revision != 2 {
			t.Fatal(ref)
		}
		c := footprintDoc()
		c.ID = "child"
		return c, nil
	}, 0)
	if e != nil || f.DerivedSessions != 2 {
		t.Fatal(f, e)
	}
}

func TestFootprintExplicitZeroAndUnknownFieldsRemainRefused(t *testing.T) {
	for _, field := range []string{"max_sessions", "session_count"} {
		var d Document
		if err := json.Unmarshal([]byte(`{"`+field+`":0}`), &d); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(d)
		if !strings.Contains(string(raw), `"`+field+`":0`) {
			t.Fatal("explicit zero lost before validation", string(raw))
		}
		if issues := FootprintIssues(d); len(issues) != 1 || issues[0].Path != field {
			t.Fatal(issues)
		}
	}
	var d Document
	if err := json.Unmarshal([]byte(`{"unknown":1}`), &d); err == nil {
		t.Fatal("Document custom decode swallowed unknown fields")
	}
}

func TestFootprintDiamondDAGAnalysisIsBounded(t *testing.T) {
	for _, scope := range []string{"molecule", "tree"} {
		d := Document{ID: "diamonds", SlotScope: scope, Start: "d0", Stages: map[string]Stage{}}
		for i := 0; i < 24; i++ {
			start := fmt.Sprintf("d%d", i)
			left := fmt.Sprintf("l%d", i)
			right := fmt.Sprintf("r%d", i)
			next := fmt.Sprintf("d%d", i+1)
			if i == 23 {
				next = "Done"
			}
			d.Stages[start] = Stage{Primitive: Branch}
			d.Stages[left] = Stage{Primitive: AgentTurn, Role: "author"}
			d.Stages[right] = Stage{Primitive: AgentTurn, Role: "author"}
			d.Edges = append(d.Edges, Edge{From: start, To: left}, Edge{From: start, To: right}, Edge{From: left, To: next}, Edge{From: right, To: next})
		}
		started := time.Now()
		_, err := DeriveFootprint(d, nil, 0)
		elapsed := time.Since(started)
		if err == nil || !strings.Contains(err.Error(), "footprint: graph") {
			t.Fatal("diamond DAG escaped bounded analysis", scope, err)
		}
		if elapsed > 3*time.Second {
			t.Fatal("diamond analysis exceeded bounded work time", scope, elapsed)
		}
	}
}
func TestFootprintRepeatedPinnedChildUsesSharedCache(t *testing.T) {
	d := footprintDoc()
	d.Stages["first"] = Stage{Primitive: SubWorkflow, Workflow: &WorkflowRef{ID: "child", Revision: 2}}
	d.Stages["second"] = d.Stages["first"]
	d.Edges = []Edge{{From: "agent", To: "first"}, {From: "first", To: "second"}, {From: "second", To: "Done"}}
	calls := 0
	f, err := DeriveFootprint(d, func(WorkflowRef) (Document, error) {
		calls++
		child := footprintDoc()
		child.ID = "child"
		child.MaxSessions = 6
		raw, _ := json.Marshal(child)
		if err := json.Unmarshal(raw, &child); err != nil {
			t.Fatal(err)
		}
		return child, nil
	}, 0)
	if err != nil || f.DerivedSessions != 3 || calls != 1 {
		t.Fatal("pinned child was re-derived or lost", f, calls, err)
	}
}

func TestFootprintLongPathBoundsCopiedRoleEntries(t *testing.T) {
	d := footprintDoc()
	d.Stages = map[string]Stage{}
	d.Edges = nil
	d.Start = "s0"
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("s%d", i)
		next := fmt.Sprintf("s%d", i+1)
		if i == 1999 {
			next = "Done"
		}
		d.Stages[id] = Stage{Primitive: AgentTurn, Role: id}
		d.Edges = append(d.Edges, Edge{From: id, To: next})
	}
	started := time.Now()
	_, err := DeriveFootprint(d, nil, 0)
	if err == nil || !strings.Contains(err.Error(), "copied entries") {
		t.Fatal("unbounded role-set copying", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("analysis did not stop promptly")
	}
	d.SlotScope = "tree"
	f, err := DeriveFootprint(d, nil, 0)
	if err != nil || f.DerivedSessions != 1 {
		t.Fatal("linear tree path should remain supported", f, err)
	}
}

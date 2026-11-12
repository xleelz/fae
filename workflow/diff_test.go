package workflow_test

import (
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

func TestDiffReportsExactlyOneCapAndOneStage(t *testing.T) {
	from := defaults.Workflow()
	to := defaults.Workflow()
	coder := to.Roles["coder"]
	coder.Cap = 3
	to.Roles["coder"] = coder
	to.Stages["operator-stop"] = workflow.Stage{Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "operator review of {task}"}
	to.Edges = append(to.Edges, workflow.Edge{From: "verdict", To: "operator-stop"})
	report, err := workflow.Diff(from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !report.From.Valid || !report.To.Valid || len(report.From.Issues)+len(report.To.Issues) != 0 {
		t.Fatalf("floor status %+v %+v", report.From, report.To)
	}
	if len(report.Changes) != 2 {
		t.Fatalf("changes %+v", report.Changes)
	}
	c, s := report.Changes[0], report.Changes[1]
	if c.Kind != "cap" || c.Op != "changed" || c.Path != "roles.coder.cap" || string(c.From) != "4" || string(c.To) != "3" {
		t.Fatalf("cap change %+v", c)
	}
	if s.Kind != "stage" || s.Op != "added" || s.Path != "stages.operator-stop" || len(s.Edges) != 1 || s.Edges[0] != (workflow.Edge{From: "verdict", To: "operator-stop"}) {
		t.Fatalf("stage change %+v", s)
	}
	// The diff is directional and identical documents have no changes.
	back, err := workflow.Diff(to, from)
	if err != nil || len(back.Changes) != 2 || back.Changes[1].Op != "removed" || len(back.Changes[1].Edges) != 1 {
		t.Fatalf("reverse %+v %v", back, err)
	}
	same, err := workflow.Diff(from, defaults.Workflow())
	if err != nil || len(same.Changes) != 0 {
		t.Fatalf("identity %+v %v", same, err)
	}
}

func TestDiffClassifiesEveryCategory(t *testing.T) {
	from := defaults.Workflow()
	to := defaults.Workflow()
	spec := to.Roles["spec"]
	spec.Routes = append(spec.Routes, workflow.Binding{Provider: "codex", Model: "m"})
	spec.Instructions += " more"
	to.Roles["spec"] = spec
	delete(to.Roles, "qa")
	to.Roles["auditor"] = from.Roles["oracle"]
	gate := to.Stages["intake-gate"]
	gate.Failure = "spec-loop"
	to.Stages["intake-gate"] = gate
	loop := to.Stages["spec-loop"]
	loop.MaxIterations++
	to.Stages["spec-loop"] = loop
	plan := to.Stages["plan"]
	plan.Phase = "implement"
	to.Stages["plan"] = plan
	to.Limits.TestRuns = 9
	edges := []workflow.Edge{}
	for _, e := range to.Edges {
		if e != (workflow.Edge{From: "verdict", To: "verdict-stop"}) {
			edges = append(edges, e)
		}
	}
	to.Edges = append(edges, workflow.Edge{From: "plan", To: "stop"})
	to.Start = "spec-loop"
	report, err := workflow.Diff(from, to)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"edges.plan->stop": "edge/added", "edges.verdict->verdict-stop": "edge/removed",
		"limits.test_runs": "limit/changed", "roles.auditor": "role/added", "roles.qa": "role/removed",
		"roles.spec.instructions": "role/changed", "roles.spec.routes": "route/changed",
		"stages.intake-gate.failure": "gate/changed", "stages.spec-loop.max_iterations": "cap/changed",
		"stages.plan.phase": "stage/changed", "start": "document/changed",
	}
	got := map[string]string{}
	for _, c := range report.Changes {
		got[c.Path] = c.Kind + "/" + c.Op
	}
	if len(got) != len(report.Changes) || len(got) != len(want) {
		t.Fatalf("changes %+v", report.Changes)
	}
	for path, kind := range want {
		if got[path] != kind {
			t.Errorf("%s: got %q want %q", path, got[path], kind)
		}
	}
	for i := 1; i < len(report.Changes); i++ {
		if report.Changes[i-1].Path > report.Changes[i].Path {
			t.Fatal("changes are not ordered")
		}
	}
	// Moving the start leaves intake unreachable, so the second side is not floor-valid.
	if !report.From.Valid || report.To.Valid || len(report.To.Issues) == 0 {
		t.Fatalf("floor status %+v", report.To)
	}
	raw, err := json.Marshal(report)
	if err != nil || !json.Valid(raw) {
		t.Fatal(err)
	}
}

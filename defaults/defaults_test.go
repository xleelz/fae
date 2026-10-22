package defaults

import (
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/workflow"
)

func TestDefaultRoleSafetyPolicy(t *testing.T) {
	d := Workflow()
	if issues := workflow.Validate(d); len(issues) != 0 {
		t.Fatal(issues)
	}
	expected := map[string]int{"spec": 2, "spec-reviewer": 2, "planner": 2, "coder": 4, "reviewer-correctness": 4, "reviewer-quality": 4, "reviewer-security": 4, "reviewer-architecture": 4, "oracle": 1, "qa": 1}
	for name, cap := range expected {
		role, ok := d.Roles[name]
		if !ok || role.Cap != cap {
			t.Errorf("role %s cap %d want %d", name, role.Cap, cap)
		}
		providers := map[string]bool{}
		for _, route := range role.Routes {
			providers[route.Provider] = true
		}
		if len(role.Routes) < 4 || len(providers) < 2 {
			t.Errorf("role %s lacks route depth/diversity", name)
		}
	}
	for _, name := range []string{"spec", "spec-reviewer", "planner"} {
		profile := d.Roles[name].Profile
		if profile.Build || profile.Test || profile.Format {
			t.Fatalf("pre-code role %s granted heavy commands", name)
		}
	}
	if d.Limits.Attempts != 20 || d.Limits.TestRuns != 8 || d.Limits.ScopeWidens != 3 {
		t.Fatal("default caps drifted", d.Limits)
	}
}
func TestDefaultCopiesAndRefusalControls(t *testing.T) {
	first := Workflow()
	first.Roles["coder"] = workflow.Role{}
	if len(workflow.Validate(first)) == 0 {
		t.Fatal("invalid role not refused")
	}
	second := Workflow()
	if len(workflow.Validate(second)) != 0 {
		t.Fatal("mutation leaked into default")
	}
	for stage, steps := range HappyScript() {
		if len(steps) == 0 {
			t.Fatal("empty fixture", stage)
		}
		for _, step := range steps {
			if len(step.Reviews) == 0 && !json.Valid(step.Output) {
				t.Fatalf("invalid fixture %s", stage)
			}
		}
	}
}

func TestReconciliationRouteMustBeBounded(t *testing.T) {
	for _, mutation := range []string{"missing-target", "wrong-primitive", "missing-edge", "unbounded"} {
		t.Run(mutation, func(t *testing.T) {
			d := Workflow()
			merge := d.Stages["merge"]
			switch mutation {
			case "missing-target":
				merge.Reverify = "absent"
				d.Stages["merge"] = merge
			case "wrong-primitive":
				merge.Primitive = workflow.VcsCommit
				d.Stages["merge"] = merge
			case "missing-edge":
				edges := d.Edges[:0]
				for _, edge := range d.Edges {
					if edge.From != "merge" || edge.To != merge.Reverify {
						edges = append(edges, edge)
					}
				}
				d.Edges = edges
			case "unbounded":
				loop := d.Stages[merge.Reverify]
				loop.MaxIterations = 0
				d.Stages[merge.Reverify] = loop
			}
			if len(workflow.Validate(d)) == 0 {
				t.Fatal("unsafe reconciliation route accepted")
			}
		})
	}
}

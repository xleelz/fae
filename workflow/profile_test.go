package workflow_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-042: tool profiles carry allowed command shims and external-effect
// grants; a stage override may only tighten them; from schema 2 an effect
// stage acts only under its role's grant (FAE-WF-001 target requirements).
func TestProfileShimsAndEffectGrants(t *testing.T) {
	shipped := defaults.Shipped()
	if len(workflow.Validate(shipped)) != 0 || !slices.Contains(shipped.Roles["planner"].Profile.Effects, defaults.ReviewMerge) {
		t.Fatal("shipped planner lacks its effect grants")
	}
	local, err := workflow.LocalProjection(shipped)
	if err != nil || len(local.Roles["planner"].Profile.Effects) != 0 {
		t.Fatal("Local projection keeps unused effect grants", err)
	}
	for name, c := range map[string]struct {
		path   string
		mutate func(*workflow.Document)
	}{
		"bad shim name": {"roles.coder.profile.shims", func(d *workflow.Document) {
			role(d, "coder", func(r *workflow.Role) { r.Profile.Shims = []string{"go test"} })
		}},
		"duplicate grant": {"roles.planner.profile.effects", func(d *workflow.Document) {
			role(d, "planner", func(r *workflow.Role) { r.Profile.Effects = append(r.Profile.Effects, defaults.ReviewMerge) })
		}},
		"ungranted effect": {"stages.review-merge.role", func(d *workflow.Document) {
			role(d, "planner", func(r *workflow.Role) { r.Profile.Effects = []string{defaults.TicketCreate} })
		}},
		"stage widens shims": {"stages.implement.profile", func(d *workflow.Document) {
			role(d, "coder", func(r *workflow.Role) { r.Profile.Shims = []string{"gofmt"} })
			stage(d, "implement", func(s *workflow.Stage) {
				s.Profile = &workflow.Profile{Allowed: []string{"handoff", "edit"}, Denied: []string{"external-effect"}, Shims: []string{"go"}}
			})
		}},
		"stage grants an effect": {"stages.oracle.profile", func(d *workflow.Document) {
			stage(d, "oracle", func(s *workflow.Stage) {
				s.Profile = &workflow.Profile{Allowed: []string{"handoff"}, Denied: []string{"external-effect"}, Effects: []string{"review_merge"}}
			})
		}},
		"missing requirements": {"target_requirements", func(d *workflow.Document) { d.TargetRequirements = nil }},
		"unrequired command": {"target_requirements.commands", func(d *workflow.Document) {
			d.TargetRequirements = &workflow.TargetRequirements{Commands: [][]string{defaults.TestCommand()}}
		}},
		"foreign command": {"target_requirements.commands[2]", func(d *workflow.Document) {
			d.TargetRequirements.Commands = append(d.TargetRequirements.Commands, []string{"make"})
		}},
	} {
		d := defaults.Shipped()
		raw, _ := json.Marshal(d)
		_ = json.Unmarshal(raw, &d) // a deep copy
		c.mutate(&d)
		if !hasIssueAt(workflow.Validate(d), c.path, "") {
			t.Errorf("%s accepted: %v", name, workflow.Validate(d))
		}
	}
	// A stage may tighten: fewer shims, no grants.
	d := defaults.Workflow()
	role(&d, "coder", func(r *workflow.Role) { r.Profile.Shims = []string{"go", "gofmt"} })
	stage(&d, "implement", func(s *workflow.Stage) {
		s.Profile = &workflow.Profile{Allowed: []string{"handoff", "edit"}, Denied: []string{"external-effect"}, Shims: []string{"gofmt"}}
	})
	if issues := workflow.Validate(d); len(issues) != 0 {
		t.Fatal(issues)
	}
	if p := workflow.EffectiveProfile(d.Roles["coder"].Profile, d.Stages["implement"]); !slices.Equal(p.Shims, []string{"gofmt"}) {
		t.Fatal("effective shims", p.Shims)
	}
	// Schema-1 content predates both sections and still loads.
	d = defaults.Shipped()
	d.SchemaVersion, d.TargetRequirements = 1, nil
	role(&d, "planner", func(r *workflow.Role) { r.Profile.Effects = nil })
	if issues := workflow.Validate(d); len(issues) != 0 {
		t.Fatal("schema-1 content refused", issues)
	}
}

func TestUnmetTargetRequirements(t *testing.T) {
	r := defaults.Workflow().TargetRequirements
	if got := workflow.UnmetTargetRequirements(r, [][]string{defaults.TestCommand(), defaults.FormatCommand()}, false, false); len(got) != 0 {
		t.Fatal(got)
	}
	strict := workflow.TargetRequirements{Commands: r.Commands, DiffClassifier: true, RequiredChecks: true}
	if got := workflow.UnmetTargetRequirements(&strict, [][]string{defaults.TestCommand()}, false, false); len(got) != 3 {
		t.Fatal("unmet", got)
	}
	if workflow.UnmetTargetRequirements(nil, nil, false, false) != nil {
		t.Fatal("schema-1 content has no requirements")
	}
}

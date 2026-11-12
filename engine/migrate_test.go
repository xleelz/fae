package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// MigrationRevisions returns a revision that changes one loop cap and one that
// bumps the Spec artifact version, so stored Spec evidence no longer fits.
func migrationRevisions() (workflow.Document, workflow.Document) {
	capped := defaults.Workflow()
	loop := capped.Stages["implement-loop"]
	loop.MaxIterations = 9
	capped.Stages["implement-loop"] = loop
	bumped := defaults.Workflow()
	spec := bumped.Artifacts["Spec"]
	spec.Version = 2
	bumped.Artifacts["Spec"] = spec
	for name, s := range bumped.Stages {
		for i := range s.Inputs {
			if s.Inputs[i].Type == "Spec" {
				s.Inputs[i].Version = 2
			}
		}
		bumped.Stages[name] = s
	}
	return capped, bumped
}

func advanceTo(t *testing.T, j engine.Job, stage string) engine.Job {
	t.Helper()
	executor := engine.NewScripted(defaults.HappyScript())
	for i := 0; i < 150 && string(j.State) != stage; i++ {
		next, d, _, e := engine.Prepare(j)
		if e != nil {
			t.Fatal(e)
		}
		j = next
		if d == nil {
			continue
		}
		r, e := executor.Execute(context.Background(), *d)
		if e != nil {
			t.Fatal(e)
		}
		if j, _, e = engine.Complete(j, d.ID, r); e != nil {
			t.Fatal(e)
		}
	}
	if string(j.State) != stage {
		t.Fatalf("did not reach %s: %s", stage, j.State)
	}
	return j
}

func TestMigrateValidatesEvidenceAtMappedPosition(t *testing.T) {
	capped, bumped := migrationRevisions()
	for _, d := range []workflow.Document{capped, bumped} {
		if issues := workflow.Validate(d); len(issues) > 0 {
			t.Fatal(issues)
		}
	}
	// needy's plan stage also requires a RedReport the task never produced.
	needy := capped
	needy.Stages = map[string]workflow.Stage{}
	for id, s := range capped.Stages {
		needy.Stages[id] = s
	}
	plan := needy.Stages["plan"]
	plan.Inputs = append(append([]workflow.Input{}, plan.Inputs...), workflow.Input{Name: "red", Type: "RedReport", Version: needy.Artifacts["RedReport"].Version})
	needy.Stages["plan"] = plan
	j := advanceTo(t, newJob(t), "plan")
	// A named remap along an edge of the new revision is a recorded transition.
	if remapped, events, e := engine.Migrate(j, capped, 2, "red-route"); e != nil || remapped.State != "red-route" || events[0].From != "plan" || events[0].To != "red-route" {
		t.Fatal("legal remap", e, remapped.State)
	}
	migrated, events, e := engine.Migrate(j, capped, 2, "")
	if e != nil {
		t.Fatal(e)
	}
	hash, _ := workflow.Hash(capped)
	if migrated.Revision != 2 || migrated.WorkflowHash != hash || migrated.State != "plan" || len(events) != 1 || events[0].Kind != "migrated" || len(migrated.Evidence) != len(j.Evidence) {
		t.Fatal(migrated.Revision, migrated.State, events)
	}
	if e = engine.Validate(migrated); e != nil {
		t.Fatal(e)
	}
	// The migrated task runs to completion on its new revision.
	if done := drive(t, migrated, engine.NewScripted(defaults.HappyScript()), false); done.State != domain.Done || done.Revision != 2 {
		t.Fatal(done.State, done.Reason)
	}
	for name, c := range map[string]struct {
		doc      workflow.Document
		revision int
		stage    string
		want     string
	}{
		"evidence misfit":     {bumped, 3, "", "evidence does not fit"},
		"missing input":       {needy, 4, "", "missing"},
		"illegal remap":       {capped, 2, "implement", "illegal transition plan -> implement"},
		"absent stage":        {capped, 2, "no-such-stage", "absent"},
		"same revision":       {capped, 1, "", "different revision"},
		"other workflow":      {func() workflow.Document { d := capped; d.ID = "other"; return d }(), 2, "", "workflow"},
		"invalid revision":    {workflow.Document{ID: capped.ID}, 2, "", "invalid"},
		"pending stage remap": {capped, 2, "plan", ""},
	} {
		source := j
		if name == "pending stage remap" {
			source, c.want = newJob(t), "Pending"
		}
		if _, _, e := engine.Migrate(source, c.doc, c.revision, c.stage); e == nil || !strings.Contains(e.Error(), c.want) {
			t.Fatalf("%s: %v", name, e)
		}
	}
	// In-flight tasks finish on their pinned revision; terminal tasks never move.
	inFlight, d, _, e := engine.Prepare(j)
	if e != nil || d == nil {
		t.Fatal(e)
	}
	if _, _, e = engine.Migrate(inFlight, capped, 2, ""); e == nil {
		t.Fatal("in-flight task migrated")
	}
	done := drive(t, j, engine.NewScripted(defaults.HappyScript()), false)
	if _, _, e = engine.Migrate(done, capped, 2, ""); e == nil {
		t.Fatal("terminal task migrated")
	}
	if pending, _, e := engine.Migrate(newJob(t), bumped, 3, ""); e != nil || pending.State != domain.Pending || pending.Revision != 3 {
		t.Fatal("pending task without evidence must migrate", e)
	}
}

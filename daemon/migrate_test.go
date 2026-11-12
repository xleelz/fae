package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func TestMigrateVerbPositiveRefusalsAndAudit(t *testing.T) {
	cfg, ctx := fixture(t)
	script := defaults.HappyScript()
	script["beta/oracle"] = []engine.ScriptStep{{Output: json.RawMessage(`{"decision":"unknown","valid_blocking":0,"rationale":"needs a human"}`)}}
	cfg.Executor = engine.NewScripted(script)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
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
	for _, doc := range []workflow.Document{capped, bumped} {
		if _, e = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); e != nil {
			t.Fatal(e)
		}
	}
	// beta runs on revision 1 to an escalation holding Spec v1 evidence.
	if _, e = call(t, d, "Admit", AdmitParams{ID: "beta", Revision: 1, Tree: "tree", AuthorizingMessage: "message"}, "beta"); e != nil {
		t.Fatal(e)
	}
	awaitState(t, d, "beta", "Escalated")
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	if _, e = call(t, d, "Admit", AdmitParams{ID: "alpha", Revision: 1, Tree: "tree", AuthorizingMessage: "message"}, "alpha"); e != nil {
		t.Fatal(e)
	}
	for name, p := range map[string]MigrateParams{
		"no authority":     {ID: "alpha", Revision: 2},
		"unknown task":     {ID: "missing", Revision: 2, AuthorizingMessage: "message"},
		"unknown revision": {ID: "alpha", Revision: 99, AuthorizingMessage: "message"},
		"unnamed revision": {ID: "alpha", AuthorizingMessage: "message"},
		"same revision":    {ID: "alpha", Revision: 1, AuthorizingMessage: "message"},
		"pending remap":    {ID: "alpha", Revision: 2, Stage: "plan", AuthorizingMessage: "message"},
		"evidence misfit":  {ID: "beta", Revision: 3, AuthorizingMessage: "message"},
		"escalation remap": {ID: "beta", Revision: 2, Stage: "plan", AuthorizingMessage: "message"},
	} {
		if _, e = call(t, d, "Migrate", p, ""); e == nil {
			t.Fatalf("%s accepted", name)
		} else if name == "evidence misfit" && !strings.Contains(e.Error(), "does not fit") {
			t.Fatalf("misfit reason %v", e)
		}
	}
	if j, e := d.store.GetJob(ctx, "beta"); e != nil || j.WorkflowRevision != 1 {
		t.Fatal("refused migration changed the task", j.WorkflowRevision, e)
	}
	var moved struct {
		ID           string `json:"id"`
		FromRevision int64  `json:"from_revision"`
		Revision     int64  `json:"revision"`
		State        string `json:"state"`
		MappedStage  string `json:"mapped_stage"`
	}
	for id, state := range map[string]string{"alpha": "Pending", "beta": "Escalated"} {
		r, e := call(t, d, "Migrate", MigrateParams{ID: id, Revision: 2, AuthorizingMessage: "message"}, "")
		decodeResult(t, r, e, &moved)
		if moved.FromRevision != 1 || moved.Revision != 2 || moved.State != state || moved.MappedStage != capped.Start {
			t.Fatalf("%s %s", id, r.Result)
		}
		j, e := d.store.GetJob(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		var env envelope
		if e = decodeEnvelope(j.Payload, &env); e != nil || j.WorkflowRevision != 2 || env.Job.Revision != 2 || env.Job.Document.Stages["implement-loop"].MaxIterations != 9 {
			t.Fatal(id, j.WorkflowRevision, env.Job.Revision, e)
		}
	}
	// A migrated pending task runs to completion on its new revision, across a restart.
	stop(t, d)
	if d, e = Start(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	if j := awaitState(t, d, "alpha", "Done"); j.WorkflowRevision != 2 {
		t.Fatal(j.WorkflowRevision)
	}
	if _, e = call(t, d, "Migrate", MigrateParams{ID: "alpha", Revision: 3, AuthorizingMessage: "message"}, ""); e == nil {
		t.Fatal("terminal task migrated")
	}
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect migration audit", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor='operator' AND action='Migrate' AND message_id='message'`).Scan(&n); e != nil {
			return e
		}
		if n != 2 {
			return fmt.Errorf("migration audit rows %d", n)
		}
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE kind='migrated' AND job_id IN ('alpha','beta')`).Scan(&n); e != nil {
			return e
		}
		if n != 2 {
			return fmt.Errorf("migration events %d", n)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}

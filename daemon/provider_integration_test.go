package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/workflow"
	"testing"
	"time"
)

func TestDaemonProviderRoutesPersistParkRestartAndFailover(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := defaults.Workflow()
	for name, role := range doc.Roles {
		role.Routes = []workflow.Binding{{Provider: "fixture", Model: "primary"}, {Provider: "fixture", Model: "fallback"}}
		doc.Roles[name] = role
	}
	for name, stage := range doc.Stages {
		if stage.Primitive == workflow.HostCommand && stage.TestRun {
			stage.Command = cfg.Live.AllowedCommands[0]
			doc.Stages[name] = stage
		}
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, model := range []string{"primary", "fallback"} {
		if err = d.store.ObserveProvider(ctx, controlplane.ProviderObservation{ID: "initial-" + model, TaskID: "health-fixture", Provider: "fixture", Model: model, Outcome: providerhealth.RateLimited, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "route-work", AuthorizingMessage: "message"}, "route-work"); err != nil {
		t.Fatal(err)
	}
	awaitPark := func() {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			s, e := d.store.GetJob(ctx, "route-work")
			if e == nil {
				var env envelope
				json.Unmarshal(s.Payload, &env)
				if env.Job.Parked && env.Job.Reason == "all-routes-unavailable" {
					if env.Job.Attempts != 0 || env.Job.Sequence != 0 {
						t.Fatal("park charged attempt", env.Job)
					}
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("did not park")
	}
	awaitPark()
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	awaitPark()
	// Deterministically advance the stored observation clock by two hours to
	// exercise decay, without an hour-long integration test or changing score.
	// The SQL plane remains authoritative across the daemon restart.
	d.mu.Lock()
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "advance decay fixture", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `UPDATE provider_health SET last_failure=? WHERE provider=? AND model=?`, now.Add(-2*time.Hour), "fixture", "fallback")
		return e
	})
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	d.signal()
	snapshot := awaitState(t, d, "route-work", "Done")
	var env envelope
	if err = json.Unmarshal(snapshot.Payload, &env); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, p := range env.Job.Provenance {
		if p.Primitive == workflow.AgentTurn || p.Primitive == workflow.Review {
			count++
			if p.Provider != "fixture" || p.Model != "fallback" {
				t.Fatal("wrong actual provenance", p)
			}
		}
	}
	if count == 0 {
		t.Fatal("no provider turns")
	}
	health, err := d.store.ProviderHealth(ctx)
	if err != nil || len(health) != 2 {
		t.Fatal(health, err)
	}
	events, err := d.store.JobEvents(ctx, "route-work", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	observed := 0
	for _, event := range events {
		if event.Kind == "provider-observation" {
			observed++
		}
	}
	if observed != count {
		t.Fatal(fmt.Sprintf("observations=%d provenance=%d", observed, count))
	}
}

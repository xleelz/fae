package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func TestDaemonStallCounterSurvivesDrainRestart(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	// This fixture does nothing except wait, and the real process is retired on
	// a stall. FAE-RT-005: with no stall_safe_replay assertion, the shipped
	// roles' restricted profiles alone prove the default replay safe.
	cfg.Live.Command = []string{"/bin/sh", "-c", "exec /bin/sleep 60", "{config}"}
	cfg.Live.StallSeconds = 1
	cfg.Live.TimeoutSeconds = 60
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := defaults.Workflow()
	for name, role := range doc.Roles {
		role.Routes = []workflow.Binding{{Provider: "fixture", Model: "first"}, {Provider: "fixture", Model: "second"}, {Provider: "fixture", Model: "third"}}
		doc.Roles[name] = role
	}
	if _, err := call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, d, "Admit", AdmitParams{ID: "stall-work", AuthorizingMessage: "message"}, "stall-work"); err != nil {
		t.Fatal(err)
	}
	wait := func(check func(engine.Job) bool) engine.Job {
		t.Helper()
		_, env := awaitJob(t, d, "stall-work", 25*time.Second, func(_ controlplane.JobSnapshot, env envelope) bool { return check(env.Job) })
		return env.Job
	}
	wait(func(j engine.Job) bool { return j.InFlight != nil })
	if _, err := call(t, d, "Drain", map[string]string{"authorizing_message": "message"}, ""); err != nil {
		t.Fatal(err)
	}
	checkpoint := wait(func(j engine.Job) bool { return j.Failures["stall-recovery"] == 1 && j.InFlight == nil })
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The third stall escalates; that ends the turn and clears its counter.
	final := wait(func(j engine.Job) bool { return j.EscalationKind == "StallExhausted" })
	if final.Failures["stall-recovery"] != 0 || final.Sequence != 3 || final.Tree != checkpoint.Tree || len(final.Provenance) != 0 || final.InFlight != nil {
		t.Fatalf("unsafe/bypassed bounded recovery: %+v", final)
	}
	pool, err := d.live.guard.host.Snapshot(context.Background())
	if err != nil || len(pool.Holders) != 0 || len(pool.Waiting) != 0 {
		t.Fatal("stall leaked leases", pool, err)
	}
	health, err := d.store.ProviderHealth(ctx)
	if err != nil || len(health) != 2 {
		t.Fatal("stall failover health", health, err)
	}
}

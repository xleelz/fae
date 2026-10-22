package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func TestDaemonStallCounterSurvivesDrainRestart(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	// This explicitly trusted fixture does nothing except wait. No worktree or
	// external effects are possible, and the real process is retired on a stall.
	cfg.Live.Command = []string{"/bin/sh", "-c", "exec /bin/sleep 60", "{config}"}
	cfg.Live.StallSeconds = 1
	cfg.Live.StallSafeReplay = true
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
		deadline := time.Now().Add(25 * time.Second)
		var env envelope
		for time.Now().Before(deadline) {
			env = envelope{} // Omitted in_flight must clear the prior polling snapshot.
			s, err := d.store.GetJob(ctx, "stall-work")
			if err == nil && json.Unmarshal(s.Payload, &env) == nil && check(env.Job) {
				return env.Job
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("stall wait: state=%s reason=%s failures=%v", env.Job.State, env.Job.Reason, env.Job.Failures)
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
	final := wait(func(j engine.Job) bool { return j.Reason == "StallExhausted" })
	if final.Failures["stall-recovery"] != 3 || final.Sequence != 3 || final.Tree != checkpoint.Tree || len(final.Provenance) != 0 || final.InFlight != nil {
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

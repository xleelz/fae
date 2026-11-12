package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/workflow"
)

// Hold the first real pre-execution failure until drain has been requested.
// This makes the restart boundary deterministic without replacing the runtime.
type startupCheckpointExecutor struct {
	inner   engine.Executor
	ready   chan struct{}
	release chan struct{}
}

func (x *startupCheckpointExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	r, err := x.inner.Execute(ctx, d)
	close(x.ready)
	select {
	case <-x.release:
	case <-ctx.Done():
	}
	return r, err
}

func TestDaemonStartupRetrySurvivesDrainRestart(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.Live.Command = []string{filepath.Join(cfg.Workspace, "missing-agent"), "{config}"}
	cfg.Live.StartSeconds = 1
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	gate := &startupCheckpointExecutor{inner: d.executor, ready: make(chan struct{}), release: make(chan struct{})}
	d.mu.Lock()
	d.executor = gate
	d.mu.Unlock()
	authorize(t, d)
	doc := defaults.Workflow()
	for name, role := range doc.Roles {
		role.Routes = []workflow.Binding{{Provider: "fixture", Model: "only-route"}}
		doc.Roles[name] = role
	}
	if _, err := call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "startup-work", AuthorizingMessage: "message"}, "startup-work"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.ready:
	case <-time.After(15 * time.Second):
		t.Fatal("startup outcome missing")
	}
	if _, err = call(t, d, "Drain", map[string]string{"authorizing_message": "message"}, ""); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	wait := func(check func(engine.Job) bool) engine.Job {
		t.Helper()
		_, current := awaitJob(t, d, "startup-work", 20*time.Second, func(_ controlplane.JobSnapshot, env envelope) bool { return check(env.Job) })
		return current.Job
	}
	starts := engine.AgentStartCountKey("intake", workflow.Binding{Provider: "fixture", Model: "only-route"})
	checkpoint := wait(func(j engine.Job) bool { return j.Failures[starts] == 1 && j.InFlight == nil })
	stop(t, d)
	// FAE-RT-004: exhaustion parks ProviderUnavailable recorded against the
	// binding; with no other route the bounded no-route park then escalates.
	cfg.RouteExhaustion = 2 * time.Second
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	final := wait(func(j engine.Job) bool { return j.EscalationKind == domain.ProviderUnavailable })
	if final.Failures[starts] != 0 || final.Sequence != 3 || final.Attempts != 0 || final.Tree != checkpoint.Tree || len(final.Provenance) != 0 || final.InFlight != nil {
		t.Fatalf("unsafe startup retry or charged budget: %+v", final)
	}
	parked := false
	for _, event := range final.History {
		parked = parked || (event.Kind == "park" && strings.HasPrefix(event.Cause, engine.ParkProviderUnavailable+": startup exhausted on fixture/only-route"))
	}
	if !parked {
		t.Fatal("startup exhaustion did not park ProviderUnavailable")
	}
	health, err := d.store.ProviderHealth(ctx)
	if err != nil || len(health) != 1 || health[0].Model != "only-route" || health[0].Outcome != providerhealth.Unavailable || health[0].Failures != 1 {
		t.Fatal("exhausted binding not recorded unavailable exactly once", health, err)
	}
	diagnostics := 0
	for _, event := range final.History {
		if event.Kind == "execution-diagnostic" {
			diagnostics++
		}
	}
	if diagnostics != 3 {
		t.Fatalf("lost startup diagnostics: %d", diagnostics)
	}
	pool, err := d.live.guard.host.Snapshot(ctx)
	if err != nil || len(pool.Holders) != 0 || len(pool.Waiting) != 0 {
		t.Fatal("startup leaked leases", pool, err)
	}
	if _, err = call(t, d, "Status", nil, ""); err != nil {
		t.Fatal("startup exhaustion stopped drive", err)
	}
}

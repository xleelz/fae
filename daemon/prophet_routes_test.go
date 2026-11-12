package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/hostguard"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xleelz/fae/providerhealth"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

var validProphetReply = json.RawMessage(`{"reply":"answer","proposals":[],"action":"","task":"","information":"","context":""}`)

func TestProphetRouteValidation(t *testing.T) {
	for _, c := range []Config{{ProphetRoutes: []workflow.Binding{{Provider: "p"}}}, {ProphetMaxFailovers: 8}, {ProphetMaxFailovers: -2}, {ProphetRoutes: []workflow.Binding{{Provider: "p", Model: "m", AutoApproval: true}}}, {ProphetRoutes: []workflow.Binding{{Provider: "p", Model: "m"}, {Provider: "p", Model: "m"}}}} {
		if _, err := prophetRoutes(c); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	r, e := prophetRoutes(Config{})
	if e != nil || len(r) != 1 || r[0].Model != "claude-opus-5-5" || r[0].Provider != "claude" {
		t.Fatal(r, e)
	}
}
func TestProphetRouteFailoverSafety(t *testing.T) {
	for _, tc := range []struct {
		name                                                                 string
		unhealthy, healthError, observeError, retired, cleanup, nonExecution bool
		refusedBeforeProvider, refusedBinding                                bool
		budget, want                                                         int
		success                                                              bool
	}{
		{name: "ordered-failover", retired: true, cleanup: true, want: 2, success: true},
		{name: "skip-unhealthy", unhealthy: true, retired: true, cleanup: true, want: 1, success: true},
		{name: "health-read-fails", healthError: true, want: 0},
		{name: "observation-uncertain", observeError: true, retired: true, cleanup: true, want: 1},
		{name: "retirement-uncertain", cleanup: true, want: 1},
		{name: "release-uncertain", retired: true, want: 1},
		{name: "disabled", retired: true, cleanup: true, budget: -1, want: 1},
		{name: "pre-provider", retired: true, cleanup: true, nonExecution: true, want: 2, success: true},
		{name: "bridge-refused-before-provider", retired: true, cleanup: true, refusedBeforeProvider: true, want: 2, success: true},
		{name: "bridge-refused-binding", retired: true, cleanup: true, refusedBinding: true, want: 2, success: true},
		// A refused binding spends no failover: with failover disabled
		// (as in "disabled", where an ordinary failure stops at once) the
		// next route still runs.
		{name: "bridge-refused-binding-no-budget", retired: true, cleanup: true, refusedBinding: true, budget: -1, want: 2, success: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, observations := 0, 0
			c := Config{ProphetRoutes: []workflow.Binding{{Provider: "p", Model: "first"}, {Provider: "p", Model: "second"}}, ProphetMaxFailovers: tc.budget}
			raw, err := routeProphet(context.Background(), c, func(context.Context) ([]providerhealth.Health, error) {
				if tc.healthError {
					return nil, errors.New("read unavailable")
				}
				if tc.unhealthy {
					return []providerhealth.Health{{Provider: "p", Model: "first", Score: 0, LastFailure: time.Now()}}, nil
				}
				return nil, nil
			}, func(_ context.Context, r workflow.Binding, n int) prophetAttempt {
				calls++
				if r.Model == "second" {
					return prophetAttempt{collected: agentruntime.Collected{Output: validProphetReply, ProcessesRetired: true}, cleanupOK: true}
				}
				var failure error = errors.New("failed")
				if tc.refusedBeforeProvider {
					failure = &agentruntime.BridgeRefusal{Class: agentruntime.RefusedBeforeProvider, Detail: "task scope refused"}
				}
				if tc.refusedBinding {
					failure = &agentruntime.BridgeRefusal{Class: agentruntime.RefusedBinding, Detail: "Claude credentials must be supplied explicitly"}
				}
				return prophetAttempt{collected: agentruntime.Collected{Output: json.RawMessage(`{}`), ProcessesRetired: tc.retired, NonExecution: tc.nonExecution}, cleanupOK: tc.cleanup, err: failure}
			}, func(_ context.Context, _ workflow.Binding, _ int, o providerhealth.Outcome) error {
				observations++
				if tc.observeError {
					return errors.New("commit unknown")
				}
				return nil
			})
			if calls != tc.want || (err == nil) != tc.success {
				t.Fatalf("calls=%d err=%v raw=%s", calls, err, raw)
			}
			if (tc.nonExecution || tc.refusedBeforeProvider || tc.refusedBinding) && observations != 1 {
				t.Fatal("pre-provider failure charged health")
			}
		})
	}
}
func TestProphetRouteBoundAndMalformed(t *testing.T) {
	c := Config{ProphetRoutes: []workflow.Binding{{Provider: "p", Model: "1"}, {Provider: "p", Model: "2"}, {Provider: "p", Model: "3"}}}
	for _, limit := range []int{0, 2} {
		c.ProphetMaxFailovers = limit
		calls := 0
		_, err := routeProphet(context.Background(), c, func(context.Context) ([]providerhealth.Health, error) { return nil, nil }, func(context.Context, workflow.Binding, int) prophetAttempt {
			calls++
			return prophetAttempt{collected: agentruntime.Collected{Output: json.RawMessage(`{}`), ProcessesRetired: true}, cleanupOK: true}
		}, func(_ context.Context, _ workflow.Binding, _ int, o providerhealth.Outcome) error {
			if o != providerhealth.MalformedOutput {
				t.Fatalf("classification %s", o)
			}
			return nil
		})
		want := 2
		if limit == 2 {
			want = 3
		}
		if err == nil || calls != want {
			t.Fatal(calls, err)
		}
	}
}
func TestProphetFailoverGuard(t *testing.T) {
	for n := 0; n < 8; n++ {
		a, b, c := n&1 != 0, n&2 != 0, n&4 != 0
		if CanFailoverProphet(a, b, c) != (n == 7) {
			t.Fatal(n)
		}
	}
}

func TestProphetRoutesNativeDurableHealthRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	c := Config{ProphetRoutes: []workflow.Binding{{Provider: "fixture", Model: "primary"}, {Provider: "fixture", Model: "fallback"}}}
	logdir := t.TempDir()
	if err = os.WriteFile(filepath.Join(logdir, "events.jsonl"), []byte(`{"type":"error","message":"HTTP 429"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, r workflow.Binding, n int) prophetAttempt {
		a := prophetAttempt{collected: agentruntime.Collected{Output: validProphetReply, ProcessesRetired: true}, cleanupOK: true}
		if r.Model == "primary" {
			a.err = errors.New("throttle")
			a.collected.LogDirectory = logdir
		}
		return a
	}
	observations := 0
	observe := func(ctx context.Context, r workflow.Binding, n int, o providerhealth.Outcome) error {
		observations++
		return d.store.ObserveProvider(ctx, controlplane.ProviderObservation{ID: fmt.Sprintf("prophet-route-test-%d", observations), TaskID: "global", Provider: r.Provider, Model: r.Model, Outcome: o, At: time.Now().UTC()})
	}
	if _, err = routeProphet(ctx, c, d.store.ProviderHealth, run, observe); err != nil || observations != 2 {
		t.Fatal(observations, err)
	}
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	if _, err = routeProphet(ctx, c, d.store.ProviderHealth, func(ctx context.Context, r workflow.Binding, n int) prophetAttempt {
		calls++
		if r.Model != "fallback" {
			t.Error("persisted unhealthy primary selected")
		}
		return run(ctx, r, n)
	}, observe); err != nil || calls != 1 || observations != 3 {
		t.Fatal(calls, observations, err)
	}
}

type prophetRouteRunner func(context.Context, agentruntime.Launch) (agentruntime.Collected, error)

func (r prophetRouteRunner) Run(ctx context.Context, l agentruntime.Launch) (agentruntime.Collected, error) {
	return r(ctx, l)
}
func TestProphetRoutesNativeLaunchIsolationAndHealth(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.ProphetRoutes = []workflow.Binding{{Provider: "fixture", Model: "primary"}, {Provider: "fixture", Model: "fallback"}}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	// Route failover and health accounting are under test, not admission: a
	// deferred host-admission refusal would fail a route over without a launch.
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) { return hostSample(1, 0.5), nil }
	calls := 0
	d.live.runner = prophetRouteRunner(func(_ context.Context, l agentruntime.Launch) (agentruntime.Collected, error) {
		calls++
		if !l.FreshContext || l.WorkflowID != "fae-prophet" || l.Role != "prophet" || l.SessionPolicy != "" || l.Binding.AutoApproval || len(l.Profile.Allowed) != 1 || l.Profile.Allowed[0] != "handoff" || l.Worktree == cfg.Workspace {
			t.Fatalf("unsafe launch %+v", l)
		}
		if calls == 1 {
			return agentruntime.Collected{ProcessesRetired: true, Output: json.RawMessage(`{}`)}, nil
		}
		if l.SessionID != "prophet-health-test-route-1" || l.Binding.Model != "fallback" {
			t.Fatalf("wrong route %+v", l)
		}
		return agentruntime.Collected{ProcessesRetired: true, Output: validProphetReply}, nil
	})
	raw, err := d.runProphet(ctx, "", "prophet-health-test", json.RawMessage(`[]`))
	if err != nil || calls != 2 || string(raw) != string(validProphetReply) {
		t.Fatal(calls, string(raw), err)
	}
	health, err := d.store.ProviderHealth(ctx)
	if err != nil || len(health) != 2 {
		t.Fatal(health, err)
	}
	for _, h := range health {
		if h.Model == "primary" && h.Outcome != providerhealth.MalformedOutput {
			t.Fatal(h)
		}
		if h.Model == "fallback" && h.Outcome != providerhealth.OK {
			t.Fatal(h)
		}
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
	"time"
)

func TestDynamicHostConfigurationAuditedRestart(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	h := hostguard.DefaultConfig()
	h.Load = 2
	if _, e = call(t, d, "RuntimeConfig", RuntimeConfig{HeavyCapacity: 3, TasksInFlight: 1, HostThresholds: &h, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	if d.effectiveTasksInFlight() != 1 || d.live.guard.dynamicHeavy.Load() != 3 || d.live.guard.hostConfig().Load != 2 {
		t.Fatal(d.dynamicConfiguration())
	}
	if _, e = call(t, d, "RuntimeConfig", RuntimeConfig{HeavyCapacity: 4, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	if d.effectiveTasksInFlight() != 1 || d.live.guard.hostConfig().Load != 2 {
		t.Fatal("partial update lost fields")
	}
	stop(t, d)
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if d.effectiveTasksInFlight() != 1 || d.live.guard.dynamicHeavy.Load() != 4 || d.live.guard.hostConfig().Load != 2 {
		t.Fatal(d.dynamicConfiguration())
	}
	raw, _ := json.Marshal(RuntimeConfig{HeavyCapacity: 2, TasksInFlight: 3})
	if e = d.store.PutMarker(ctx, controlplane.Audit{Actor: "test", Action: "external limits", Outcome: "ok"}, "runtime-config", raw); e != nil {
		t.Fatal(e)
	}
	if e = d.dispatch(); e != nil {
		t.Fatal(e)
	}
	if d.effectiveTasksInFlight() != 3 || d.live.guard.dynamicHeavy.Load() != 2 {
		t.Fatal("round did not refresh")
	}
	for _, bad := range []RuntimeConfig{{HeavyCapacity: -1}, {TasksInFlight: -2}, {TasksInFlight: 1000001}, {HostThresholds: &hostguard.Config{}}} {
		bad.AuthorizingMessage = "message"
		if _, e = call(t, d, "RuntimeConfig", bad, ""); e == nil {
			t.Fatal("invalid config", bad)
		}
	}
}

func TestDynamicHostThresholdsAffectAdmission(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) {
		return hostguard.Sample{Memory: hostguard.Reading{Value: 100 * float64(hostguard.GiB), Known: true}, Swap: hostguard.Reading{Value: 1, Known: true}, LoadPerCore: hostguard.Reading{Value: 3, Known: true}}, nil
	}
	h := hostguard.DefaultConfig()
	h.Load = 2
	if _, e = call(t, d, "RuntimeConfig", RuntimeConfig{HostThresholds: &h, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	dispatch := engine.Dispatch{Config: workflow.Stage{Primitive: workflow.HostCommand}}
	if release, e := d.live.guard.Acquire(ctx, dispatch, execution.Binding{}, engine.Session{ID: "strict"}); e == nil || release != nil || !strings.Contains(e.Error(), "HostPressure") {
		t.Fatal("strict threshold did not refuse", e)
	}
	h.Load = 4
	if _, e = call(t, d, "RuntimeConfig", RuntimeConfig{HostThresholds: &h, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	release, e := d.live.guard.Acquire(ctx, dispatch, execution.Binding{}, engine.Session{ID: "relaxed"})
	if e != nil {
		t.Fatal(e)
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
	assertHostCounts(t, d.live.guard, 0, 0)
}

type dynamicLimitExecutor struct{ started chan string }

func (e dynamicLimitExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	e.started <- d.TaskID
	<-ctx.Done()
	return engine.Result{}, ctx.Err()
}
func TestDynamicTasksLimitNextRoundPreservesActiveWork(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	ex := dynamicLimitExecutor{make(chan string, 4)}
	d.mu.Lock()
	d.executor = ex
	d.mu.Unlock()
	if _, e = call(t, d, "RuntimeConfig", RuntimeConfig{TasksInFlight: 1, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"limit-one", "limit-two"} {
		if _, e = call(t, d, "Admit", AdmitParams{ID: id, AuthorizingMessage: "message"}, id); e != nil {
			t.Fatal(e)
		}
	}
	select {
	case <-ex.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first task not started")
	}
	select {
	case id := <-ex.started:
		t.Fatal("cap exceeded", id)
	case <-time.After(200 * time.Millisecond):
	}
	if _, e = call(t, d, "RuntimeConfig", RuntimeConfig{TasksInFlight: 2, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	select {
	case <-ex.started:
	case <-time.After(5 * time.Second):
		t.Fatal("raised cap did not start second task")
	}
	if _, e = call(t, d, "RuntimeConfig", RuntimeConfig{TasksInFlight: 1, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	active := len(d.active)
	d.mu.Unlock()
	if active != 2 {
		t.Fatal("lowering limit retired active work", active)
	}
}

package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/loops"
	"github.com/xleelz/fae/providerhealth"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

func TestLoopLiveStartupRelaunchBridgeFixture(t *testing.T) {
	if os.Getenv("LOOP_STARTUP_FIXTURE") != "1" {
		return
	}
	raw, err := os.ReadFile(os.Args[len(os.Args)-1])
	if err != nil {
		os.Exit(31)
	}
	var c agentruntime.BridgeConfig
	if json.Unmarshal(raw, &c) != nil || !strings.HasPrefix(c.WorkflowID, "fae-loop:") {
		os.Exit(32)
	}
	firstStart := !strings.Contains(c.SessionID, "-attempt-1-start-")
	switch c.Binding.Model {
	case "not-started-once":
		if firstStart {
			if agentruntime.RecordBridgeRefusal(c, agentruntime.ProviderNotStarted, "provider exited before acknowledging") != nil {
				os.Exit(33)
			}
			os.Exit(agentruntime.BridgeRefusalExit)
		}
	case "refused-once":
		if firstStart {
			if agentruntime.RecordBridgeRefusal(c, agentruntime.RefusedBeforeProvider, "local preflight refusal") != nil {
				os.Exit(34)
			}
			os.Exit(agentruntime.BridgeRefusalExit)
		}
	case "always-not-started":
		if agentruntime.RecordBridgeRefusal(c, agentruntime.ProviderNotStarted, "provider never acknowledged") != nil {
			os.Exit(35)
		}
		os.Exit(agentruntime.BridgeRefusalExit)
	}
	handoff, _ := json.Marshal(agentruntime.Handoff{DispatchID: c.DispatchID, SessionID: c.SessionID, TaskID: c.TaskID, Output: json.RawMessage(`{"message":"continued"}`)})
	if os.WriteFile(c.Handoff, handoff, 0600) != nil {
		os.Exit(36)
	}
	os.Exit(0)
}

// FAE-RT-004: a live provider-loop turn that fails before starting
// (provider-not-started or refused-before-provider) is relaunched automatically
// without counting the failed start as an execution, and parks uncertain only
// when all relaunch attempts fail before starting.
func TestLoopLiveStartupNotExecutedRelaunchesAndExhausts(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	exe, _ := os.Executable()
	cfg.Live.Command = []string{exe, "-test.run=^TestLoopLiveStartupRelaunchBridgeFixture$", "--", "{config}"}
	cfg.Live.Environment = map[string]string{"LOOP_STARTUP_FIXTURE": "1"}
	cfg.Live.PersistentLoops = true
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	for _, spec := range []loops.Spec{
		{ID: "recover-start", Kind: "loop", Role: "oracle", Prompt: "continue", IntervalSeconds: 60, MaxIterations: 2, Binding: workflow.Binding{Provider: "fixture", Model: "not-started-once"}},
		{ID: "recover-refused", Kind: "loop", Role: "oracle", Prompt: "continue", IntervalSeconds: 60, MaxIterations: 2, Binding: workflow.Binding{Provider: "fixture", Model: "refused-once"}},
		{ID: "exhaust-start", Kind: "loop", Role: "oracle", Prompt: "continue", MaxIterations: 2, Binding: workflow.Binding{Provider: "fixture", Model: "always-not-started"}},
	} {
		if _, err = call(t, d, "Schedule", ScheduleParams{Action: "create", Spec: &spec, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	waitActiveTurn := func(id string) loops.Schedule {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			s := loopRead(t, d, id)
			if s.Iterations == 1 && !s.InFlight {
				return s
			}
			if s.State == "uncertain" {
				t.Fatalf("%s unexpectedly parked uncertain: %+v", id, s)
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s did not complete first turn: %+v", id, loopRead(t, d, id))
		return loops.Schedule{}
	}
	for _, id := range []string{"recover-start", "recover-refused"} {
		s := waitActiveTurn(id)
		if s.State != "active" || s.Iterations != 1 || s.StartupNotExecuted != 1 || s.Dispatches != 1 {
			t.Fatalf("%s state=%q iterations=%d startup_not_executed=%d dispatches=%d", id, s.State, s.Iterations, s.StartupNotExecuted, s.Dispatches)
		}
	}
	exhausted := loopWait(t, d, "exhaust-start", "uncertain")
	if exhausted.State != "uncertain" || exhausted.Iterations != 0 || exhausted.StartupNotExecuted != loops.StartAttempts {
		t.Fatalf("exhaust-start: %+v", exhausted)
	}
	health, err := d.store.ProviderHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]providerhealth.Health{}
	for _, h := range health {
		outcomes[h.Provider+"/"+h.Model] = h
	}
	if h := outcomes["fixture/not-started-once"]; h.Outcome != providerhealth.OK {
		t.Fatalf("recover-start health: %+v", h)
	}
	if h := outcomes["fixture/refused-once"]; h.Outcome != providerhealth.OK {
		t.Fatalf("recover-refused health: %+v", h)
	}
	if h := outcomes["fixture/always-not-started"]; h.Outcome != providerhealth.Unavailable {
		t.Fatalf("exhaust-start health: %+v", h)
	}
}

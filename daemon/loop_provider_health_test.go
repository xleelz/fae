package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/loops"
	"github.com/xleelz/fae/providerhealth"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// TestLoopProviderHealthBridgeFixture is a provider-loop bridge: a binding
// whose model is "throttled" logs a structured rate-limit error and fails,
// any other answers.
func TestLoopProviderHealthBridgeFixture(t *testing.T) {
	if os.Getenv("LOOP_HEALTH_FIXTURE") != "1" {
		return
	}
	raw, e := os.ReadFile(os.Args[len(os.Args)-1])
	if e != nil {
		os.Exit(31)
	}
	var c agentruntime.BridgeConfig
	if json.Unmarshal(raw, &c) != nil || !strings.HasPrefix(c.WorkflowID, "fae-loop:") {
		os.Exit(32)
	}
	if c.Binding.Model == "throttled" {
		if os.WriteFile(filepath.Join(c.LogDirectory, "events.jsonl"), []byte(`{"type":"error","error":{"code":"rate_limit_exceeded","message":"429 Too Many Requests"}}`+"\n"), 0600) != nil {
			os.Exit(33)
		}
		os.Exit(1)
	}
	handoff, _ := json.Marshal(agentruntime.Handoff{DispatchID: c.DispatchID, SessionID: c.SessionID, TaskID: c.TaskID, Output: json.RawMessage(`{"message":"continued"}`)})
	if os.WriteFile(c.Handoff, handoff, 0600) != nil {
		os.Exit(34)
	}
	os.Exit(0)
}

// FAE-RT-009: a provider-loop turn records its provider's health like a
// workflow turn: a throttled loop turn charges its binding RateLimited, which
// makes the binding ineligible for workflow routing, and an answered loop
// turn records ok, each as a provider observation of the loop's session.
func TestLoopTurnsRecordProviderHealth(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	exe, _ := os.Executable()
	cfg.Live.Command = []string{exe, "-test.run=^TestLoopProviderHealthBridgeFixture$", "--", "{config}"}
	cfg.Live.Environment = map[string]string{"LOOP_HEALTH_FIXTURE": "1"}
	cfg.Live.PersistentLoops = true
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	for _, spec := range []loops.Spec{
		{ID: "answered", Kind: "loop", Role: "oracle", Prompt: "continue", MaxIterations: 1, Binding: workflow.Binding{Provider: "fixture", Model: "answering"}},
		{ID: "throttled", Kind: "loop", Role: "oracle", Prompt: "continue", MaxIterations: 1, Binding: workflow.Binding{Provider: "fixture", Model: "throttled"}},
	} {
		if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Spec: &spec, AuthorizingMessage: "message"}, ""); e != nil {
			t.Fatal(e)
		}
	}
	loopWait(t, d, "answered", "completed")
	loopWait(t, d, "throttled", "uncertain")
	health, e := d.store.ProviderHealth(ctx)
	if e != nil {
		t.Fatal(e)
	}
	outcomes := map[string]providerhealth.Health{}
	for _, h := range health {
		outcomes[h.Provider+"/"+h.Model] = h
	}
	if h := outcomes["fixture/answering"]; h.Outcome != providerhealth.OK {
		t.Fatalf("answered loop turn recorded %+v", h)
	}
	if h := outcomes["fixture/throttled"]; h.Outcome != providerhealth.RateLimited || h.Eligible(h.LastFailure) {
		t.Fatalf("throttled loop turn recorded %+v", h)
	}
	observations, e := d.store.ProviderObservations(ctx, 0, 100)
	if e != nil {
		t.Fatal(e)
	}
	lines := map[string]string{}
	for _, line := range observations {
		lines[line.Session] = line.Line
	}
	if !strings.Contains(lines["loop-answered-1-attempt-1"], "outcome "+string(providerhealth.OK)) || !strings.Contains(lines["loop-throttled-1-attempt-1"], "outcome "+string(providerhealth.RateLimited)) {
		t.Fatalf("loop observations %v", lines)
	}
}

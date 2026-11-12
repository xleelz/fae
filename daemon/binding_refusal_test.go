package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/workflow"
)

// TestLiveBindingRefusalBridgeFixture is the bridge
// TestDaemonBindingRefusalFailsOverToTheNextRoute launches: the production
// provider.Run with the Claude Code protocol over a scripted CLI, exiting with
// provider.ExitCode as cmd/fae-claude-bridge does. While the marker named by
// LIVE_BINDING_NO_CREDENTIALS exists, the target environment has no Claude
// credentials.
func TestLiveBindingRefusalBridgeFixture(t *testing.T) {
	if os.Getenv("LIVE_BINDING_BRIDGE") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := os.Stat(os.Getenv("LIVE_BINDING_NO_CREDENTIALS")); err == nil {
		_ = os.Unsetenv("CLAUDE_CODE_OAUTH_TOKEN")
	}
	c := provider.Config{Provider: "claude", Binary: os.Getenv("LIVE_BINDING_CLI"), Files: []string{"answer.go"}, Timeout: 30 * time.Second}
	err := provider.Run(ctx, c, os.Args[len(os.Args)-1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(provider.ExitCode(err))
}

// FAE-RT-010, FAE-WF-082, FAE-RT-009 through the daemon and the shipped Claude
// bridge. The role's first route is a Codex binding, which this Claude-only
// instance's bridge refuses before any provider runs: the turn fails over to
// the second route and the task reaches Done, with no stage failure, no
// health charged to either binding, the refusal as an event and a paired
// supervision line, and the second binding in the provenance. With no Claude
// credentials in the target environment, the bridge refuses the second
// route too, so the task parks (all-routes-unavailable, Appendix B) with both
// refusals named in its status reason and withheld as route_refused, charging
// no attempt and running no provider, instead of retrying one refused binding
// to SameGate or escalating. Later rounds of the same daemon leave the park
// as it is, and an operator resume tries both routes again in place. With
// credentials supplied, the daemon restart that starts a new route generation
// resumes the task in place and it reaches Done.
func TestDaemonBindingRefusalFailsOverToTheNextRoute(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	scratch := filepath.Dir(cfg.Root)
	cli, log, noCredentials := filepath.Join(scratch, "claude"), filepath.Join(scratch, "claude.log"), filepath.Join(scratch, "no-credentials")
	if err := os.WriteFile(cli, []byte(refusalCLIScript(log)), 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Live.Command = []string{exe, "-test.run=^TestLiveBindingRefusalBridgeFixture$", "--", "{config}"}
	cfg.Live.Environment = map[string]string{"LIVE_BINDING_BRIDGE": "1", "LIVE_BINDING_CLI": cli, "LIVE_BINDING_NO_CREDENTIALS": noCredentials, "CLAUDE_CODE_OAUTH_TOKEN": "fixture-token"}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	accepted := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "intake", Path: []string{"accepted"}}, {Op: "literal", Value: true}}}
	routes := []workflow.Binding{{Provider: "codex", Model: "fixture-codex", Effort: "medium"}, {Provider: "claude", Model: "fixture-model"}}
	doc := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "binding-refusal", Start: "intake", Limits: workflow.Limits{Attempts: 8, TestRuns: 2, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{"intake": {Instructions: "Accept the item.", Briefs: map[string]string{"default": "Item {{task_id}}."}, Routes: routes, Scope: "task-bound", Cap: 1, Outputs: []string{"IntakeReport"}}},
		Artifacts: map[string]workflow.ArtifactType{"IntakeReport": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["accepted"],"additionalProperties":false,"properties":{"accepted":{"type":"boolean"}}}`)}},
		Stages:    map[string]workflow.Stage{"intake": {Primitive: workflow.AgentTurn, Role: "intake", Output: "IntakeReport"}, "intake-gate": {Primitive: workflow.Gate, Expr: &accepted, Failure: "stop"}, "stop": {Primitive: workflow.Escalate}},
		Edges:     []workflow.Edge{{From: "intake", To: "intake-gate"}, {From: "intake-gate", To: "Done"}, {From: "intake-gate", To: "stop"}}})
	if issues := workflow.Validate(doc); len(issues) != 0 {
		t.Fatal(issues)
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	run := func(id string) engine.Job {
		t.Helper()
		if _, err := call(t, d, "Admit", AdmitParams{ID: id, RunID: id, WorkflowID: doc.ID, AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(id, err)
		}
		_, e := awaitJob(t, d, id, 2*time.Minute, func(s controlplane.JobSnapshot, e envelope) bool {
			return s.State == "Done" || e.Job.Parked && !deferredPark(e) || e.Job.State.Terminal() || e.Job.State == domain.Escalated
		})
		return e.Job
	}
	// kinds counts the task's history events by kind; observations counts
	// its recorded provider observations.
	kinds := func(j engine.Job) map[string]int {
		n := map[string]int{}
		for _, event := range j.History {
			n[event.Kind]++
		}
		return n
	}
	observations := func(id string) int {
		t.Helper()
		events, err := d.store.JobEvents(ctx, id, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, event := range events {
			if event.Kind == "provider-observation" {
				n++
			}
		}
		return n
	}
	refusedCause := func(j engine.Job) string {
		for _, event := range j.History {
			if event.Kind == "route-refused" {
				return event.Cause
			}
		}
		return ""
	}

	// The Codex route is refused; the turn runs on the Claude route.
	j := run("binding-failover")
	k := kinds(j)
	if j.State != "Done" || j.Parked || k["failure"] != 0 || k["route-refused"] != 1 || j.Attempts != 1 {
		t.Fatalf("refused first route did not fail over: state=%s parked=%v reason=%q attempts=%d events %v", j.State, j.Parked, j.Reason, j.Attempts, k)
	}
	if cause := refusedCause(j); !strings.HasPrefix(cause, "codex/fixture-codex (effort medium): bridge refused the binding before the provider ran: ") || !strings.Contains(cause, `does not serve "codex" bindings`) {
		t.Fatalf("refusal cause %q", cause)
	}
	if len(j.Provenance) != 1 || j.Provenance[0].Provider != "claude" || j.Provenance[0].Model != "fixture-model" {
		t.Fatalf("provenance %+v", j.Provenance)
	}
	if n := observations("binding-failover"); n != 1 {
		t.Fatalf("provider observations %d, want the Claude turn's only", n)
	}
	health, err := d.store.ProviderHealth(ctx)
	if err != nil || len(health) != 1 || health[0].Provider != "claude" || health[0].Failures != 0 {
		t.Fatalf("binding health %+v %v", health, err)
	}
	if raw, _ := os.ReadFile(log); strings.Count(string(raw), "turn\n") != 1 {
		t.Fatalf("provider turns %q, want 1", raw)
	}
	if raw, err := os.ReadFile(d.layout.Log); err != nil || !strings.Contains(string(raw), `component=task route-refused cause="codex/fixture-codex (effort medium): `) {
		t.Fatalf("no supervision line for the refusal: %v", err)
	}

	// No credentials: the bridge refuses every route, so the task parks,
	// naming both refusals, and no provider runs.
	if err = os.WriteFile(noCredentials, nil, 0600); err != nil {
		t.Fatal(err)
	}
	j = run("binding-exhausted")
	k = kinds(j)
	if j.State != "intake" || !j.Parked || j.EscalationKind != "" || k["failure"] != 0 || k["route-refused"] != 2 || k["park"] != 1 || j.Attempts != 0 {
		t.Fatalf("every route refused: state=%s parked=%v kind=%s reason=%q attempts=%d events %v", j.State, j.Parked, j.EscalationKind, j.Reason, j.Attempts, k)
	}
	for _, want := range []string{"all-routes-unavailable: every route for role intake was refused by the bridge before its provider ran: ", `codex/fixture-codex (effort medium): bridge refused the binding before the provider ran: this claude bridge does not serve "codex" bindings`, "claude/fixture-model: bridge refused the binding before the provider ran: Claude credentials must be supplied explicitly", "restart the daemon or resume the task"} {
		if !strings.Contains(j.Reason, want) {
			t.Fatalf("park reason lacks %q: %s", want, j.Reason)
		}
	}
	if n := observations("binding-exhausted"); n != 0 {
		t.Fatalf("refused bindings charged %d provider observations", n)
	}
	if raw, _ := os.ReadFile(log); strings.Count(string(raw), "turn\n") != 1 {
		t.Fatalf("provider turns %q, want none for the refused task", raw)
	}
	// Status shows the reason, the round withholds the task as
	// route_refused, and later rounds change nothing.
	parked, err := d.store.GetJob(ctx, "binding-exhausted")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		w := d.withheld.Load()
		if d.statusTasks()["binding-exhausted"].Reason == j.Reason && w != nil && w.Tasks["binding-exhausted"] == withheldRouteRefused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %+v withheld %+v", d.statusTasks()["binding-exhausted"], w)
		}
		d.signal()
		time.Sleep(50 * time.Millisecond)
	}
	for range 3 {
		round := d.withheld.Load().Round
		for d.withheld.Load().Round == round && time.Now().Before(deadline) {
			d.signal()
			time.Sleep(20 * time.Millisecond)
		}
	}
	if again, err := d.store.GetJob(ctx, "binding-exhausted"); err != nil || again.Version != parked.Version {
		t.Fatalf("a refused park changed within its route generation: version %d -> %d %v", parked.Version, again.Version, err)
	}
	// An operator resume (fae resume --task) forgets the refusals in place:
	// with credentials still missing, both routes are tried and refused
	// again, and the task parks again, still charged nothing.
	if _, err = call(t, d, "ResumeRun", ResumeRunParams{Task: "binding-exhausted", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	_, e := awaitJob(t, d, "binding-exhausted", 2*time.Minute, func(s controlplane.JobSnapshot, e envelope) bool {
		return e.Job.Parked && kinds(e.Job)["route-refused"] == 4 || e.Job.State == domain.Escalated
	})
	if j = e.Job; j.State != "intake" || !strings.HasPrefix(j.Reason, "all-routes-unavailable: every route for role intake was refused") || kinds(j)["park"] != 2 || j.Attempts != 0 {
		t.Fatalf("resume of the refused park: state=%s reason=%q attempts=%d events %v", j.State, j.Reason, j.Attempts, kinds(j))
	}

	// Credentials supplied and the daemon restarted: the new route
	// generation tries every route again, so the task resumes at intake
	// and reaches Done on the Claude route.
	if err = os.Remove(noCredentials); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	_, e = awaitJob(t, d, "binding-exhausted", 2*time.Minute, func(s controlplane.JobSnapshot, e envelope) bool {
		return s.State == "Done" || e.Job.State.Terminal() || e.Job.State == domain.Escalated
	})
	j = e.Job
	k = kinds(j)
	if j.State != "Done" || k["failure"] != 0 || k["route-refused"] != 5 || j.Attempts != 1 || len(j.Provenance) != 1 || j.Provenance[0].Provider != "claude" {
		t.Fatalf("restart did not resume the parked task: state=%s reason=%q attempts=%d events %v provenance %+v", j.State, j.Reason, j.Attempts, k, j.Provenance)
	}
	if raw, _ := os.ReadFile(log); strings.Count(string(raw), "turn\n") != 2 {
		t.Fatalf("provider turns %q, want the resumed task's", raw)
	}
}

// FAE-RT-004 through the daemon and the shipped Claude bridge: a provider CLI
// that exits before acknowledging the prompt (here on its first route's
// model, every time) never parks UncertainDispatch. The engine relaunches it
// within the agent-start bound (3 starts, no attempt charged), then parks
// ProviderUnavailable recorded against that binding, and the turn fails over
// to the second route and reaches Done.
func TestDaemonProviderThatNeverAcknowledgesRetriesThenFailsOver(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	scratch := filepath.Dir(cfg.Root)
	cli, log := filepath.Join(scratch, "claude"), filepath.Join(scratch, "claude.log")
	silent := "#!/bin/sh\nfor arg in \"$@\"; do case \"$arg\" in --model=fixture-silent) cat > /dev/null; printf 'silent\\n' >> '" + log + "'; exit 1;; esac; done\n"
	if err := os.WriteFile(cli, []byte(silent+strings.TrimPrefix(refusalCLIScript(log), "#!/bin/sh\n")), 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Live.Command = []string{exe, "-test.run=^TestLiveBindingRefusalBridgeFixture$", "--", "{config}"}
	cfg.Live.Environment = map[string]string{"LIVE_BINDING_BRIDGE": "1", "LIVE_BINDING_CLI": cli, "LIVE_BINDING_NO_CREDENTIALS": filepath.Join(scratch, "unused"), "CLAUDE_CODE_OAUTH_TOKEN": "fixture-token"}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	accepted := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "intake", Path: []string{"accepted"}}, {Op: "literal", Value: true}}}
	routes := []workflow.Binding{{Provider: "claude", Model: "fixture-silent"}, {Provider: "claude", Model: "fixture-model"}}
	doc := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "never-acknowledged", Start: "intake", Limits: workflow.Limits{Attempts: 8, TestRuns: 2, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{"intake": {Instructions: "Accept the item.", Briefs: map[string]string{"default": "Item {{task_id}}."}, Routes: routes, Scope: "task-bound", Cap: 1, Outputs: []string{"IntakeReport"}}},
		Artifacts: map[string]workflow.ArtifactType{"IntakeReport": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["accepted"],"additionalProperties":false,"properties":{"accepted":{"type":"boolean"}}}`)}},
		Stages:    map[string]workflow.Stage{"intake": {Primitive: workflow.AgentTurn, Role: "intake", Output: "IntakeReport"}, "intake-gate": {Primitive: workflow.Gate, Expr: &accepted, Failure: "stop"}, "stop": {Primitive: workflow.Escalate}},
		Edges:     []workflow.Edge{{From: "intake", To: "intake-gate"}, {From: "intake-gate", To: "Done"}, {From: "intake-gate", To: "stop"}}})
	if issues := workflow.Validate(doc); len(issues) != 0 {
		t.Fatal(issues)
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "never-acknowledged", RunID: "never-acknowledged", WorkflowID: doc.ID, AuthorizingMessage: "message"}, "never-acknowledged"); err != nil {
		t.Fatal(err)
	}
	_, e := awaitJob(t, d, "never-acknowledged", 2*time.Minute, func(s controlplane.JobSnapshot, e envelope) bool {
		return s.State == "Done" || e.Job.Parked && e.Job.Reason == "UncertainDispatch" || e.Job.State.Terminal() || e.Job.State == domain.Escalated
	})
	j := e.Job
	k := map[string]int{}
	var park string
	for _, event := range j.History {
		k[event.Kind]++
		if event.Kind == "park" {
			park = event.Cause
		}
	}
	if j.State != "Done" || k["startup-retry"] != 2 || k["park"] != 1 || park != "ProviderUnavailable: startup exhausted on claude/fixture-silent" || k["failure"] != 0 || j.Attempts != 1 {
		t.Fatalf("never-acknowledging provider: state=%s reason=%q attempts=%d park %q events %v", j.State, j.Reason, j.Attempts, park, k)
	}
	if len(j.Provenance) != 1 || j.Provenance[0].Model != "fixture-model" {
		t.Fatalf("provenance %+v", j.Provenance)
	}
	if raw, _ := os.ReadFile(log); strings.Count(string(raw), "silent\n") != 3 || strings.Count(string(raw), "turn\n") != 1 {
		t.Fatalf("provider launches %q, want 3 silent starts and 1 turn", raw)
	}
	health, err := d.store.ProviderHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	charged := map[string]int64{}
	for _, h := range health {
		charged[h.Model] = h.Failures
	}
	if charged["fixture-silent"] != 1 || charged["fixture-model"] != 0 {
		t.Fatalf("binding health %+v: want ProviderUnavailable recorded once against the silent binding", health)
	}
}

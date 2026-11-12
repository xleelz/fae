package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/workflow"
)

// TestLiveRefusalBridgeFixture is the bridge
// TestDaemonBridgeRefusalsRetryWithoutParking launches: the production
// provider.Run with the Claude Code protocol over a scripted CLI, exiting with
// provider.ExitCode as cmd/fae-claude-bridge does. While the marker named by
// LIVE_REFUSAL_LOCAL exists, one turn pins a scope its task lacks, so the
// bridge refuses it before the provider runs.
func TestLiveRefusalBridgeFixture(t *testing.T) {
	if os.Getenv("LIVE_REFUSAL_BRIDGE") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := provider.Config{Provider: "claude", Binary: os.Getenv("LIVE_REFUSAL_CLI"), Files: []string{"answer.go"}, Timeout: 30 * time.Second}
	if marker := os.Getenv("LIVE_REFUSAL_LOCAL"); marker != "" && os.Remove(marker) == nil {
		c.Files, c.TaskScopesJSON = nil, `{"another-task":{"files":["answer.go"],"test_file":"answer_test.go"}}`
	}
	err := provider.Run(ctx, c, os.Args[len(os.Args)-1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(provider.ExitCode(err))
}

// refusalCLIScript emulates Claude Code's stream-json. It logs each turn.
// While log.malformed exists it answers once, and while log.always exists
// every time, with output that fails the artifact schema.
func refusalCLIScript(log string) string {
	return `#!/bin/sh
cat > /dev/null
printf 'turn\n' >> '` + log + `'
output='{"output":{"accepted":true},"edits":[]}'
if [ -e '` + log + `.malformed' ]; then rm -f '` + log + `.malformed'; output='{"output":{"accepted":"yes"},"edits":[]}'; fi
[ -e '` + log + `.always' ] && output='{"output":{"accepted":"yes"},"edits":[]}'

sid=32345678-1234-4234-8234-123456789abc
printf '{"type":"system","subtype":"init","session_id":"%s","tools":["StructuredOutput"],"mcp_servers":[]}\n' "$sid"
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"%s","structured_output":%s}\n' "$sid" "$output"
`
}

// FAE-WF-034, FAE-RT-004/RT-009 through the daemon: a provider answer the
// shipped bridge refuses (here a schema failure) records MalformedOutput
// against the binding and retries the stage, and a turn the bridge refuses
// before the provider runs is refused before launch and charges nothing, not
// even the attempt (FAE-RT-004).
// Neither parks UncertainDispatch; both tasks reach Done. An answer refused
// every time retries within the stage's bound and then escalates SameGate.
func TestDaemonBridgeRefusalsRetryWithoutParking(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	scratch := filepath.Dir(cfg.Root)
	cli, log, local := filepath.Join(scratch, "claude"), filepath.Join(scratch, "claude.log"), filepath.Join(scratch, "refuse-locally")
	if err := os.WriteFile(cli, []byte(refusalCLIScript(log)), 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Live.Command = []string{exe, "-test.run=^TestLiveRefusalBridgeFixture$", "--", "{config}"}
	cfg.Live.Environment = map[string]string{"LIVE_REFUSAL_BRIDGE": "1", "LIVE_REFUSAL_CLI": cli, "LIVE_REFUSAL_LOCAL": local, "CLAUDE_CODE_OAUTH_TOKEN": "fixture-token"}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	accepted := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "intake", Path: []string{"accepted"}}, {Op: "literal", Value: true}}}
	doc := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "refusal-path", Start: "intake", Limits: workflow.Limits{Attempts: 8, TestRuns: 2, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{"intake": {Instructions: "Accept the item.", Briefs: map[string]string{"default": "Item {{task_id}}."}, Routes: []workflow.Binding{{Provider: "claude", Model: "fixture-model"}, {Provider: "claude", Model: "fixture-model-b"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"IntakeReport"}}},
		Artifacts: map[string]workflow.ArtifactType{"IntakeReport": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["accepted"],"additionalProperties":false,"properties":{"accepted":{"type":"boolean"}}}`)}},
		Stages:    map[string]workflow.Stage{"intake": {Primitive: workflow.AgentTurn, Role: "intake", Output: "IntakeReport"}, "intake-gate": {Primitive: workflow.Gate, Expr: &accepted, Failure: "stop"}, "stop": {Primitive: workflow.Escalate}},
		Edges:     []workflow.Edge{{From: "intake", To: "intake-gate"}, {From: "intake-gate", To: "Done"}, {From: "intake-gate", To: "stop"}}})
	if issues := workflow.Validate(doc); len(issues) != 0 {
		t.Fatal(issues)
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	// run admits one task and waits until it is Done, parked, escalated or
	// terminal.
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
	// outcomes lists the provider observations recorded for a task, in order.
	outcomes := func(id string) []providerhealth.Outcome {
		t.Helper()
		events, err := d.store.JobEvents(ctx, id, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		var out []providerhealth.Outcome
		for _, event := range events {
			var payload struct {
				Observation controlplane.ProviderObservation `json:"observation"`
			}
			if event.Kind == "provider-observation" && json.Unmarshal(event.Payload, &payload) == nil {
				out = append(out, payload.Observation.Outcome)
			}
		}
		return out
	}
	// failures lists the task's recorded stage failures.
	failures := func(j engine.Job) []string {
		var out []string
		for _, event := range j.History {
			if event.Kind == "failure" {
				out = append(out, event.Cause)
			}
		}
		return out
	}

	// A refused provider answer: MalformedOutput, health charged, retried.
	if err = os.WriteFile(log+".malformed", nil, 0600); err != nil {
		t.Fatal(err)
	}
	j := run("refusal-output")
	if j.State != "Done" || j.Parked {
		t.Fatalf("malformed provider answer not retried: state=%s parked=%v reason=%s", j.State, j.Parked, j.Reason)
	}
	if f := failures(j); len(f) != 1 || !strings.HasPrefix(f[0], "MalformedOutput: bridge refused the provider's output: ") {
		t.Fatalf("failures %q", f)
	}
	if got := outcomes("refusal-output"); fmt.Sprint(got) != fmt.Sprint([]providerhealth.Outcome{providerhealth.MalformedOutput, providerhealth.OK}) {
		t.Fatalf("provider observations %v", got)
	}

	// A turn refused before the provider ran: refused before launch, nothing
	// charged (no attempt, no provider), retried; the provider runs once, on
	// the retry, the task's only attempt.
	if err = os.WriteFile(local, nil, 0600); err != nil {
		t.Fatal(err)
	}
	j = run("refusal-local")
	if j.State != "Done" || j.Parked {
		t.Fatalf("local bridge refusal not retried: state=%s parked=%v reason=%s", j.State, j.Parked, j.Reason)
	}
	if f := failures(j); len(f) != 1 || !strings.HasPrefix(f[0], "RefusedBeforeLaunch: bridge refused the turn before the provider ran: pinned provider task scope required") || j.Attempts != 1 {
		t.Fatalf("failures %q attempts %d", f, j.Attempts)
	}
	if got := outcomes("refusal-local"); fmt.Sprint(got) != fmt.Sprint([]providerhealth.Outcome{providerhealth.OK}) {
		t.Fatalf("local refusal charged to the provider: %v", got)
	}
	if raw, _ := os.ReadFile(log); strings.Count(string(raw), "turn\n") != 3 {
		t.Fatalf("provider turns %q, want 2 for the malformed task and 1 for the local refusal", raw)
	}
	health, err := d.store.ProviderHealth(ctx)
	if err != nil || len(health) != 1 || health[0].Provider != "claude" || health[0].Model != "fixture-model" || health[0].Failures != 1 {
		t.Fatalf("binding health %+v %v", health, err)
	}

	// The same refusal every time: bounded retries, then a typed escalation.
	// Two refusals make a binding ineligible (40 points each); the second
	// route keeps one eligible, so the stage's same-reason bound ends it.
	if err = os.WriteFile(log+".always", nil, 0600); err != nil {
		t.Fatal(err)
	}
	j = run("refusal-persistent")
	f := failures(j)
	if j.State != domain.Escalated || j.EscalationKind != domain.SameGate || len(f) != engine.SameGateRepeats+1 {
		t.Fatalf("persistent refusal: state=%s kind=%s failures=%q", j.State, j.EscalationKind, f)
	}
	for _, cause := range f {
		if cause != f[0] || !strings.HasPrefix(cause, "MalformedOutput: bridge refused the provider's output: ") {
			t.Fatalf("refusal reasons differ or are untyped: %q", f)
		}
	}
	if got := outcomes("refusal-persistent"); len(got) != len(f) || slices.ContainsFunc(got, func(o providerhealth.Outcome) bool { return o != providerhealth.MalformedOutput }) {
		t.Fatalf("persistent refusal observations %v", got)
	}
	if health, err = d.store.ProviderHealth(ctx); err != nil || len(health) != 2 || health[0].Failures+health[1].Failures != 1+int64(len(f)) {
		t.Fatalf("binding health %+v %v", health, err)
	}
}

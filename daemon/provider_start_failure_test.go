package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// startFailureDaemon starts a live daemon whose bridge is the shipped Claude
// bridge (TestLiveBindingRefusalBridgeFixture) over the scripted CLI, and
// installs doc.
func startFailureDaemon(t *testing.T, script string, doc workflow.Document) (*Daemon, func()) {
	t.Helper()
	cfg, ctx, _ := liveDaemonFixture(t)
	scratch := filepath.Dir(cfg.Root)
	cli := filepath.Join(scratch, "claude")
	if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
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
	authorize(t, d)
	if issues := workflow.Validate(doc); len(issues) != 0 {
		stop(t, d)
		t.Fatal(issues)
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		stop(t, d)
		t.Fatal(err)
	}
	return d, func() { stop(t, d) }
}

// runStartFailureTask admits one task and waits until it is Done, parked
// (other than a host deferral), escalated or terminal.
func runStartFailureTask(t *testing.T, d *Daemon, id, workflowID string) engine.Job {
	t.Helper()
	if _, err := call(t, d, "Admit", AdmitParams{ID: id, RunID: id, WorkflowID: workflowID, AuthorizingMessage: "message"}, id); err != nil {
		t.Fatal(id, err)
	}
	_, e := awaitJob(t, d, id, 3*time.Minute, func(s controlplane.JobSnapshot, e envelope) bool {
		return s.State == "Done" || e.Job.Parked && !deferredPark(e) && e.Job.Reason != engine.ParkProviderUnavailable || e.Job.State.Terminal() || e.Job.State == domain.Escalated
	})
	return e.Job
}

// startFailureHealth maps each binding's model to its recorded failures.
func startFailureHealth(t *testing.T, d *Daemon) map[string]int64 {
	t.Helper()
	health, err := d.store.ProviderHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	failures := map[string]int64{}
	for _, h := range health {
		failures[h.Model] = h.Failures
	}
	return failures
}

// FAE-RT-004 (operator ruling 2026-10-29), FAE-WF-033, FAE-RT-009 through the
// daemon and the shipped Claude bridge: a review stage whose second
// reviewer's binding never acknowledges the prompt does not park
// UncertainDispatch. Each start failure charges nothing (that session never
// ran) and runs both reviewers again; the binding's third failed start parks
// ProviderUnavailable against the second reviewer's binding (charged once,
// the first reviewer's binding not at all), and route selection moves that
// reviewer to its next route, where the stage completes on the task's first
// attempt.
func TestDaemonLaterReviewerThatNeverAcknowledgesRerunsEveryReviewer(t *testing.T) {
	log := filepath.Join(t.TempDir(), "claude.log")
	silent := "#!/bin/sh\nfor arg in \"$@\"; do case \"$arg\" in --model=fixture-silent) cat > /dev/null; printf 'silent\\n' >> '" + log + "'; exit 1;; esac; done\n"
	accepted := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "review", Path: []string{"accepted"}}, {Op: "literal", Value: true}}}
	role := func(routes ...workflow.Binding) workflow.Role {
		return workflow.Role{Instructions: "Review the item.", Briefs: map[string]string{"default": "Item {{task_id}}."}, Routes: routes, Scope: "task-bound", Cap: 1, Outputs: []string{"Verdict"}}
	}
	doc := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "later-reviewer-start", Start: "review", Limits: workflow.Limits{Attempts: 8, TestRuns: 2, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{"a": role(workflow.Binding{Provider: "claude", Model: "fixture-model"}), "b": role(workflow.Binding{Provider: "claude", Model: "fixture-silent"}, workflow.Binding{Provider: "claude", Model: "fixture-other"})},
		Artifacts: map[string]workflow.ArtifactType{"Verdict": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["accepted"],"additionalProperties":false,"properties":{"accepted":{"type":"boolean"}}}`)}},
		Stages:    map[string]workflow.Stage{"review": {Primitive: workflow.Review, Role: "a", Reviewers: []string{"a", "b"}, Output: "Verdict"}, "gate": {Primitive: workflow.Gate, Expr: &accepted, Failure: "stop"}, "stop": {Primitive: workflow.Escalate}},
		Edges:     []workflow.Edge{{From: "review", To: "gate"}, {From: "gate", To: "Done"}, {From: "gate", To: "stop"}}})
	d, done := startFailureDaemon(t, silent+strings.TrimPrefix(refusalCLIScript(log), "#!/bin/sh\n"), doc)
	defer done()
	j := runStartFailureTask(t, d, "later-reviewer-start", doc.ID)
	k := map[string]int{}
	var park string
	for _, event := range j.History {
		k[event.Kind]++
		if event.Kind == "park" {
			park = event.Cause
		}
	}
	if j.State != "Done" || k["startup-retry"] != 2 || k["park"] != 1 || !strings.HasPrefix(park, "ProviderUnavailable: startup exhausted on claude/fixture-silent [after 1 of 2 review sessions ran") || k["failure"] != 0 || j.Attempts != 1 {
		t.Fatalf("later reviewer that never acknowledges: state=%s reason=%q attempts=%d park %q events %v", j.State, j.Reason, j.Attempts, park, k)
	}
	if len(j.Provenance) != 2 || j.Provenance[0].Model != "fixture-model" || j.Provenance[1].Model != "fixture-other" {
		t.Fatalf("provenance %+v", j.Provenance)
	}
	if raw, _ := os.ReadFile(log); strings.Count(string(raw), "silent\n") != 3 || strings.Count(string(raw), "turn\n") != 5 {
		t.Fatalf("provider launches %q, want 3 silent starts and 5 turns (4 by the first reviewer, 1 on the second's next route)", raw)
	}
	if charged := startFailureHealth(t, d); charged["fixture-silent"] != 1 || charged["fixture-model"] != 0 || charged["fixture-other"] != 0 {
		t.Fatalf("binding health %v: want ProviderUnavailable recorded once against the silent reviewer's binding", charged)
	}
}

// FAE-WF-034 through the daemon and the shipped Claude bridge: a provider CLI
// that announced its session and then exited with a failure and no
// structured error is charged malformed-output health and retries the stage
// within its bound, instead of parking UncertainDispatch; the task reaches
// Done. The same failure every time escalates SameGate after the stage's
// same-reason bound: its reason names no per-turn path.
func TestDaemonProviderFailedWithoutAnswerRetriesTheStage(t *testing.T) {
	log := filepath.Join(t.TempDir(), "claude.log")
	failing := `#!/bin/sh
if [ -e '` + log + `.fail' ] || [ -e '` + log + `.always' ]; then
 rm -f '` + log + `.fail'
 cat > /dev/null
 printf 'failed\n' >> '` + log + `'
 printf '%s\n' '{"type":"system","subtype":"init","session_id":"42345678-1234-4234-8234-123456789abc","tools":["StructuredOutput"],"mcp_servers":[]}'
 echo 'fatal: stream closed' >&2
 exit 3
fi
`
	accepted := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "intake", Path: []string{"accepted"}}, {Op: "literal", Value: true}}}
	doc := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "provider-failed", Start: "intake", Limits: workflow.Limits{Attempts: 8, TestRuns: 2, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{"intake": {Instructions: "Accept the item.", Briefs: map[string]string{"default": "Item {{task_id}}."}, Routes: []workflow.Binding{{Provider: "claude", Model: "fixture-model"}, {Provider: "claude", Model: "fixture-model-b"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"IntakeReport"}}},
		Artifacts: map[string]workflow.ArtifactType{"IntakeReport": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["accepted"],"additionalProperties":false,"properties":{"accepted":{"type":"boolean"}}}`)}},
		Stages:    map[string]workflow.Stage{"intake": {Primitive: workflow.AgentTurn, Role: "intake", Output: "IntakeReport"}, "intake-gate": {Primitive: workflow.Gate, Expr: &accepted, Failure: "stop"}, "stop": {Primitive: workflow.Escalate}},
		Edges:     []workflow.Edge{{From: "intake", To: "intake-gate"}, {From: "intake-gate", To: "Done"}, {From: "intake-gate", To: "stop"}}})
	d, done := startFailureDaemon(t, failing+strings.TrimPrefix(refusalCLIScript(log), "#!/bin/sh\n"), doc)
	defer done()
	failures := func(j engine.Job) []string {
		var out []string
		for _, event := range j.History {
			if event.Kind == "failure" {
				out = append(out, event.Cause)
			}
		}
		return out
	}
	const reason = "MalformedOutput: the provider failed without an answer, and the bridge wrote nothing: provider exited with a failure after acknowledging the prompt: exit status 3"

	if err := os.WriteFile(log+".fail", nil, 0600); err != nil {
		t.Fatal(err)
	}
	j := runStartFailureTask(t, d, "provider-failed-once", doc.ID)
	if j.State != "Done" || j.Parked {
		t.Fatalf("provider that failed after acknowledging not retried: state=%s parked=%v reason=%s", j.State, j.Parked, j.Reason)
	}
	if f := failures(j); len(f) != 1 || f[0] != reason {
		t.Fatalf("failures %q", f)
	}
	if raw, _ := os.ReadFile(log); strings.Count(string(raw), "failed\n") != 1 || strings.Count(string(raw), "turn\n") != 1 {
		t.Fatalf("provider launches %q", raw)
	}
	if charged := startFailureHealth(t, d); charged["fixture-model"] != 1 {
		t.Fatalf("binding health %v: want malformed output recorded once", charged)
	}

	if err := os.WriteFile(log+".always", nil, 0600); err != nil {
		t.Fatal(err)
	}
	j = runStartFailureTask(t, d, "provider-failed-always", doc.ID)
	f := failures(j)
	if j.State != domain.Escalated || j.EscalationKind != domain.SameGate || len(f) != engine.SameGateRepeats+1 || slices.ContainsFunc(f, func(cause string) bool { return cause != reason }) {
		t.Fatalf("persistent provider failure: state=%s kind=%s parked=%v reason=%q failures=%q", j.State, j.EscalationKind, j.Parked, j.Reason, f)
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/workflow"
)

// FAE-RT-004, FAE-WF-074 (operator ruling 2026-10-29: a run never executed
// doesn't count towards the budget): a targeted test run whose task scope
// can't hold it (a derived scope with no RedReport naming the RED identity)
// is refused before anything starts. The live executor types it a refusal
// before launch, and the engine charges the task neither an attempt nor one
// of its test runs; the refusal is the stage's recorded failure, and SameGate
// bounds its repeats.
func TestLiveTestRunRefusedBeforeStartChargesNothing(t *testing.T) {
	pass := workflow.Expr{Op: "and", Args: []workflow.Expr{
		{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "run", Path: []string{"exit_code"}}, {Op: "literal", Value: 0}}},
		{Op: "ge", Args: []workflow.Expr{{Op: "field", Stage: "run", Path: []string{"executed_count"}}, {Op: "literal", Value: 1}}}}}
	doc := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "refused-run", Start: "run", Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{},
		Artifacts: map[string]workflow.ArtifactType{"EngineRun": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}},
		Stages: map[string]workflow.Stage{
			"run":  {Primitive: workflow.HostCommand, Output: "EngineRun", Command: []string{"make", "test"}, Weight: 1, TimeoutSeconds: 30, TestRun: true, TestShape: "runner"},
			"gate": {Primitive: workflow.Gate, Expr: &pass, Failure: "stop"},
			"stop": {Primitive: workflow.Escalate, Reason: "the run failed"},
		},
		Edges: []workflow.Edge{{From: "run", To: "gate"}, {From: "gate", To: "Done"}, {From: "gate", To: "stop"}}})
	if issues := workflow.Validate(doc); len(issues) != 0 {
		t.Fatal(issues)
	}
	job, err := engine.NewLive("task", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{jobs: map[string]controlplane.JobSnapshot{}}
	x := &liveExecutor{daemon: daemon, broker: &effects.Broker{}}
	daemon.live = x
	for n := 1; n <= engine.SameGateRepeats+1; n++ {
		var d *engine.Dispatch
		for d == nil {
			if job, d, _, err = engine.Prepare(job); err != nil {
				t.Fatal(err)
			}
		}
		if job.Attempts != 1 || job.TestRuns != 1 {
			t.Fatalf("refusal %d: dispatch charged attempts %d test runs %d", n, job.Attempts, job.TestRuns)
		}
		raw, err := json.Marshal(envelope{Admission: AdmitParams{ID: "task", DeclaredFiles: []string{"answer.go", "answer_test.go"}}, Job: job})
		if err != nil {
			t.Fatal(err)
		}
		daemon.jobs["task"] = controlplane.JobSnapshot{ID: "task", Payload: raw}
		r, err := x.Execute(t.Context(), *d)
		if !isScopeRefusal(err) || r.Class != engine.ClassRefusedBeforeLaunch || r.Session != d.Sessions[0].ID {
			t.Fatalf("refusal %d: class %q err %v", n, r.Class, err)
		}
		r.Detail = err.Error()
		next, events, err := engine.Complete(job, d.ID, r)
		if err != nil {
			t.Fatal(err)
		}
		job = next
		if job.Attempts != 0 || job.TestRuns != 0 {
			t.Fatalf("refusal %d charged: attempts %d test runs %d", n, job.Attempts, job.TestRuns)
		}
		failed := false
		for _, e := range events {
			failed = failed || e.Kind == "failure" && strings.HasPrefix(e.Cause, "RefusedBeforeLaunch: task scope refused: ")
		}
		if !failed {
			t.Fatalf("refusal %d: events %+v", n, events)
		}
	}
	if !job.Escalated("SameGate") {
		t.Fatalf("the fourth refusal before launch: %s %s %q", job.State, job.EscalationKind, job.EscalationDetail)
	}
}

// FAE-RT-004, FAE-WF-074, FAE-OBS-006 (re-audit 11: a test run that never
// started, refused by the broker before exec, was classed uncertain, and the
// operator's redispatch charged an attempt and a test run again): a test run
// whose allowed command cannot be prepared (here its executable does not
// exist) is refused before exec and, through the daemon, before launch: it
// charges neither an attempt nor a test run, each refusal has a paired
// supervision line, the fourth escalates SameGate, and the refusal is
// recorded against the effect intent, which recovery reads as a
// non-execution.
func TestDaemonTestRunRefusedBeforeExecChargesNothing(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	missing := []string{"w13-no-such-test-tool", "test", "./..."}
	cfg.Live.AllowedCommands = append(cfg.Live.AllowedCommands, missing)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	pass := workflow.Expr{Op: "and", Args: []workflow.Expr{
		{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "run", Path: []string{"exit_code"}}, {Op: "literal", Value: 0}}},
		{Op: "ge", Args: []workflow.Expr{{Op: "field", Stage: "run", Path: []string{"executed_count"}}, {Op: "literal", Value: 1}}}}}
	doc := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "refused-before-exec", Start: "run", Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{},
		Artifacts: map[string]workflow.ArtifactType{"EngineRun": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}},
		Stages: map[string]workflow.Stage{
			"run":  {Primitive: workflow.HostCommand, Output: "EngineRun", Command: missing, Weight: 1, TimeoutSeconds: 30, TestRun: true},
			"gate": {Primitive: workflow.Gate, Expr: &pass, Failure: "stop"},
			"stop": {Primitive: workflow.Escalate, Reason: "the run failed"},
		},
		Edges: []workflow.Edge{{From: "run", To: "gate"}, {From: "gate", To: "Done"}, {From: "gate", To: "stop"}}})
	if issues := workflow.Validate(doc); len(issues) != 0 {
		t.Fatal(issues)
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "never-exec", RunID: "never-exec", WorkflowID: doc.ID, AuthorizingMessage: "message"}, "never-exec"); err != nil {
		t.Fatal(err)
	}
	awaitThroughDeferred(t, d, "never-exec", "Escalated", 3*time.Minute)
	e := liveEnvelope(t, d, "never-exec")
	j := e.Job
	refusals := 0
	for _, event := range j.History {
		if event.Kind == "failure" && strings.HasPrefix(event.Cause, "RefusedBeforeLaunch: refused before exec: ") {
			refusals++
		}
		// A deferral for host pressure parks before any run is asked for;
		// under real host pressure it is expected.
		if event.Kind == "park" && event.Cause != "infrastructure-deferred" {
			t.Fatalf("a run that never started parked: %+v", event)
		}
	}
	if !j.Escalated("SameGate") || j.Attempts != 0 || j.TestRuns != 0 || j.Counts["run"] != 0 || refusals != engine.SameGateRepeats+1 || !strings.Contains(j.EscalationDetail, "refused before launch each time, charging no attempt") {
		t.Fatalf("state=%s kind=%s %q attempts=%d test runs=%d visits=%d refusals=%d", j.State, j.EscalationKind, j.EscalationDetail, j.Attempts, j.TestRuns, j.Counts["run"], refusals)
	}
	if raw, err := os.ReadFile(d.layout.Log); err != nil || strings.Count(string(raw), `component=task refused-before-launch cause="refused before exec: `) != refusals {
		t.Fatalf("supervision lines paired with the %d refusals: %v\n%s", refusals, err, raw)
	}
	// The refusal recorded against a recorded intent is positive evidence of
	// non-execution for recovery, as a missing started marker is.
	w, err := d.live.manager.Ensure(ctx, "never-exec", "never-exec", cfg.Live.Base)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := engine.Dispatch{ExecutionKind: "live", ID: "never-exec/refused/1", TaskID: "never-exec", Stage: "run", Tree: j.Tree, Config: doc.Stages["run"], Sessions: []engine.Session{{ID: "never-exec/refused/1/session/0"}}, CreatedAt: time.Now().UTC()}
	intent, err := d.live.effectIntent(ctx, dispatch, w, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.effectNonExecution(ctx, dispatch) {
		t.Fatal("an intent with its started marker read as a non-execution")
	}
	if err = d.live.recordEffectRefusal(ctx, intent, errors.New("refused before exec: fixture")); err != nil {
		t.Fatal(err)
	}
	if !d.effectNonExecution(ctx, dispatch) {
		t.Fatal("a refusal recorded against the intent did not read as a non-execution")
	}
}

// FAE-HOST-006, FAE-RT-004, FAE-WF-033 through the daemon and the shipped
// Claude bridge (re-audit 11: no daemon-level test of a later session's
// deferral existed, and the deferral reran every earlier reviewer every 5 s
// without bound): the host turns busy after the first reviewer ran. The
// second reviewer's admission is deferred; the executor asks again within
// its wait rather than discarding the first review, then the dispatch parks
// infrastructure-deferred, uncharged, with the turn's deferrals counted. Once
// the host is calm the stage runs both reviewers and the task reaches Done on
// its first attempt.
func TestDaemonLaterSessionDeferralWaitsForTheHostThenParksCounted(t *testing.T) {
	scratch := t.TempDir()
	log, busy := filepath.Join(scratch, "claude.log"), filepath.Join(scratch, "host-busy")
	// The first provider turn makes the host busy for every later admission.
	script := "#!/bin/sh\n[ -e '" + log + "' ] || : > '" + busy + "'\n" + strings.TrimPrefix(refusalCLIScript(log), "#!/bin/sh\n")
	accepted := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "review", Path: []string{"accepted"}}, {Op: "literal", Value: true}}}
	role := func() workflow.Role {
		return workflow.Role{Instructions: "Review the item.", Briefs: map[string]string{"default": "Item {{task_id}}."}, Routes: []workflow.Binding{{Provider: "claude", Model: "fixture-model"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"Verdict"}}
	}
	doc := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "later-session-deferral", Start: "review", Limits: workflow.Limits{Attempts: 8, TestRuns: 2, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{"a": role(), "b": role()},
		Artifacts: map[string]workflow.ArtifactType{"Verdict": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["accepted"],"additionalProperties":false,"properties":{"accepted":{"type":"boolean"}}}`)}},
		Stages:    map[string]workflow.Stage{"review": {Primitive: workflow.Review, Role: "a", Reviewers: []string{"a", "b"}, Output: "Verdict"}, "gate": {Primitive: workflow.Gate, Expr: &accepted, Failure: "stop"}, "stop": {Primitive: workflow.Escalate}},
		Edges:     []workflow.Edge{{From: "review", To: "gate"}, {From: "gate", To: "Done"}, {From: "gate", To: "stop"}}})
	d, done := startFailureDaemon(t, script, doc)
	defer done()
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) {
		if _, err := os.Stat(busy); err == nil {
			return hostSample(50, 0.5), nil
		}
		return hostSample(1, 0.5), nil
	}
	x := d.executor.(*execution.Executor)
	x.LaterSessionAdmissionWait, x.LaterSessionAdmissionRetry = 400*time.Millisecond, 20*time.Millisecond
	if _, err := call(t, d, "Admit", AdmitParams{ID: "deferred-later", RunID: "deferred-later", WorkflowID: doc.ID, AuthorizingMessage: "message"}, "deferred-later"); err != nil {
		t.Fatal(err)
	}
	_, e := awaitJob(t, d, "deferred-later", 3*time.Minute, func(s controlplane.JobSnapshot, e envelope) bool {
		return deferredPark(e) && e.Job.Failures["admission-deferred:review"] >= 1 || e.Job.State == domain.Escalated || e.Job.State.Terminal() || e.Job.Reason == "UncertainDispatch"
	})
	if !deferredPark(e) || e.Job.Attempts != 0 || e.Job.Counts["review"] != 0 || len(e.Job.Evidence) != 0 || e.Job.InFlight != nil {
		t.Fatalf("later session deferred: state=%s parked=%v reason=%q attempts=%d visits=%d evidence=%d", e.Job.State, e.Job.Parked, e.Job.Reason, e.Job.Attempts, e.Job.Counts["review"], len(e.Job.Evidence))
	}
	if raw, _ := os.ReadFile(log); strings.Count(string(raw), "turn\n") != 1 {
		t.Fatalf("provider turns %q, want the first reviewer's only: the executor's wait must not rerun it", raw)
	}
	if err := os.Remove(busy); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "deferred-later", "Done")
	e = liveEnvelope(t, d, "deferred-later")
	parks, failures := 0, 0
	for _, event := range e.Job.History {
		switch {
		case event.Kind == "park" && event.Cause == "infrastructure-deferred":
			parks++
		case event.Kind == "failure" || event.Kind == "park":
			failures++
		}
	}
	if e.Job.Attempts != 1 || len(e.Job.Provenance) != 2 || parks < 1 || failures != 0 {
		t.Fatalf("after the host calmed: attempts=%d provenance=%d deferral parks=%d other parks or failures=%d", e.Job.Attempts, len(e.Job.Provenance), parks, failures)
	}
	if raw, _ := os.ReadFile(log); strings.Count(string(raw), "turn\n") != 3 {
		t.Fatalf("provider turns %q, want 3: the first reviewer once for nothing, then both", raw)
	}
}

// FAE-OBS-006: the failure event a dispatch refused before launch records
// has a paired supervision line, naming the refusal without its class
// prefix; another failure has none.
func TestRefusedBeforeLaunchFailureEventHasAPairedLine(t *testing.T) {
	line := func(cause string) (string, bool) {
		payload, _ := json.Marshal(engine.Event{Kind: "failure", Stage: "run", DispatchID: "task/wf/3", Attempt: 1, Cause: cause})
		return pairedEventLine(controlplane.Event{JobID: "task", Payload: payload})
	}
	if got, ok := line("RefusedBeforeLaunch: refused before exec: exec: \"w13-no-such-test-tool\": executable file not found"); !ok || got != "refused-before-launch cause=\"refused before exec: exec: \\\"w13-no-such-test-tool\\\": executable file not found\" attempt=1 task=\"task\" stage=\"run\" turn=\"task/wf/3\"\n" {
		t.Fatalf("paired line %q ok=%v", got, ok)
	}
	if got, ok := line("MalformedOutput: object required"); ok {
		t.Fatalf("a malformed-output failure got a paired line %q", got)
	}
}

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	agentruntime "github.com/xleelz/fae/runtime"
)

// redRecoveryMarker is the restart information of
// TestDaemonLiveDerivedBrokenRedReportRecoversAfterRestart; the fixture's
// turns read it from their operator context.
const redRecoveryMarker = "red-recovery: the RED may now share the scope with other_test.go"

// TestLiveRedRecoveryBridgeFixture answers the turns of the derived-scope RED
// recovery tests. It writes the RED test and the RedReport of test-first
// turns, the Spec of the restart test and its first-pass Oracle verdict
// itself; every other turn is the live daemon bridge fixture's.
//
// LIVE_RED_MODE=retry: a test-first turn whose brief does not say its
// previous RedReport was refused reports a RedReport naming no test file.
//
// LIVE_RED_MODE=restart: before the restart (no redRecoveryMarker in the
// operator context) the RedReport names answer_test.go and other_test.go
// while only answer_test.go is in scope, and the Oracle escalates; after it
// the Spec also declares other_test.go, which breaks that RedReport, and the
// new RedReport names answer_test.go alone.
func TestLiveRedRecoveryBridgeFixture(t *testing.T) {
	if os.Getenv("LIVE_RED_RECOVERY") != "1" {
		return
	}
	var c agentruntime.BridgeConfig
	raw, err := os.ReadFile(os.Args[len(os.Args)-1])
	if err != nil || json.Unmarshal(raw, &c) != nil {
		os.Exit(41)
	}
	var blocks []struct{ Kind, Stage, Text string }
	if json.Unmarshal(c.Brief, &blocks) != nil {
		os.Exit(42)
	}
	stage, operator := "", ""
	for _, b := range blocks {
		if b.Kind == "stage" {
			stage = b.Stage
		}
		if b.Kind == "operator_context" {
			operator = b.Text
		}
	}
	mode, restarted := os.Getenv("LIVE_RED_MODE"), strings.Contains(operator, redRecoveryMarker)
	happy := defaults.HappyScript()
	var output map[string]any
	switch {
	case stage == "test-first":
		if json.Unmarshal(happy[stage][0].Output, &output) != nil {
			os.Exit(43)
		}
		source := liveTestSource
		if prior, _ := os.ReadFile(filepath.Join(c.Worktree, "answer.go")); strings.Contains(string(prior), "return 42") {
			// The task was implemented before the restart: the new RED needs 43.
			source = strings.Replace(liveTestSource, "!= 42", "!= 43", 1)
		}
		declared := []string{"answer.go", "answer_test.go"}
		if mode == "retry" && !strings.Contains(operator, "RedReport was refused") {
			declared = []string{"answer.go"}
		}
		if mode == "restart" && !restarted {
			declared = []string{"answer.go", "answer_test.go", "other_test.go"}
		}
		output["declared_files"], output["source"] = declared, source
		if os.WriteFile(filepath.Join(c.Worktree, "answer_test.go"), []byte(source), 0600) != nil {
			os.Exit(44)
		}
	case stage == "spec" && mode == "restart" && restarted:
		if json.Unmarshal(happy[stage][0].Output, &output) != nil {
			os.Exit(43)
		}
		output["declared_files"] = []string{"answer.go", "answer_test.go", "other_test.go"}
	case stage == "oracle" && mode == "restart" && !restarted:
		output = map[string]any{"approved": false, "findings": []any{map[string]any{"finding": "the operator must decide", "valid": true, "severity": "blocking", "target": "unknown", "rationale": "escalate this pass"}}, "rationale": "escalate this pass"}
	default:
		TestLiveDaemonBridgeFixture(t)
		os.Exit(45)
	}
	body, _ := json.Marshal(output)
	out, _ := json.Marshal(agentruntime.Handoff{DispatchID: c.DispatchID, SessionID: c.SessionID, TaskID: c.TaskID, Output: body})
	if os.WriteFile(c.Handoff, out, 0600) != nil {
		os.Exit(46)
	}
	os.Exit(0)
}

// redRecoveryFixture is a derived-scope live daemon whose bridge is
// TestLiveRedRecoveryBridgeFixture in the given mode.
func redRecoveryFixture(t *testing.T, mode string) (*Daemon, context.Context) {
	t.Helper()
	cfg, ctx, _ := liveDaemonFixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Live.Command = []string{exe, "-test.run=^TestLiveRedRecoveryBridgeFixture$", "--", "{config}"}
	cfg.Live.Environment["LIVE_RED_RECOVERY"] = "1"
	cfg.Live.Environment["LIVE_RED_MODE"] = mode
	cfg.Live.Environment["LIVE_DAEMON_RESTART_TEST"] = "1"
	cfg.Live.Files, cfg.Live.TestFile, cfg.Live.TestID, cfg.Live.ExpectedFailure, cfg.Live.CommitMessage = nil, "", "", "", ""
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(t, d) })
	authorize(t, d)
	return d, ctx
}

// awaitRedRecoverySettled waits until the task is Done or Escalated.
func awaitRedRecoverySettled(t *testing.T, d *Daemon, id string) envelope {
	t.Helper()
	guard := newHangGuard(3 * time.Minute)
	version := int64(-1)
	for {
		j, err := d.store.GetJob(context.Background(), id)
		var e envelope
		if err == nil && json.Unmarshal(j.Payload, &e) == nil {
			if e.Job.State == domain.Done || e.Job.State == domain.Escalated {
				return e
			}
			if e.Job.State.Terminal() {
				t.Fatalf("task %s ended %s: %s", id, e.Job.State, e.Job.Reason)
			}
			if j.Version != version {
				version = j.Version
				guard.progressed()
			}
		}
		select {
		case <-d.done:
			t.Fatalf("daemon stopped: %v", d.err)
		default:
		}
		if !guard.observe(err == nil && deferredPark(e)) {
			t.Fatalf("task %s did not settle: state=%s err=%v", id, e.Job.State, err)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// redRecoveryFailures are the task's recorded stage failures, by stage.
func redRecoveryFailures(e envelope) map[string][]string {
	out := map[string][]string{}
	for _, h := range e.Job.History {
		if h.Kind == "failure" {
			out[h.Stage] = append(out[h.Stage], h.Cause)
		}
	}
	return out
}

// A derived RED's malformed RedReport is refused when its test-first turn
// completes (Appendix C step 7 loops back to step 5): the turn's RED file is
// restored, test-first is retried and told why, and the task reaches Done.
// No RED run is spent on, or refused for, the malformed report.
func TestDaemonLiveDerivedBrokenRedReportRetriedAtTestFirst(t *testing.T) {
	d, _ := redRecoveryFixture(t, "retry")
	if _, err := call(t, d, "Admit", AdmitParams{ID: "red-retry", Title: "Make answer return 42", Content: "answer() must return 42", AuthorizingMessage: "message"}, "red-retry"); err != nil {
		t.Fatal(err)
	}
	e := awaitRedRecoverySettled(t, d, "red-retry")
	failures := redRecoveryFailures(e)
	if e.Job.State != domain.Done {
		t.Fatalf("task ended %s (%s): failures %v", e.Job.State, e.Job.Reason, failures)
	}
	if len(failures["test-first"]) != 1 || !strings.Contains(failures["test-first"][0], "ValidationFailed") || !strings.Contains(failures["test-first"][0], "RedReport refused at turn completion") || len(failures["run-red"]) != 0 {
		t.Fatalf("malformed RedReport not refused once at test-first: %v", failures)
	}
	reports := 0
	for _, a := range e.Job.Evidence {
		if a.Type == "RedReport" {
			reports++
			if files, _ := liveEvidencePaths(a.Fields["declared_files"]); len(files) != 2 {
				t.Fatalf("malformed RedReport recorded: %v", a.Fields["declared_files"])
			}
		}
	}
	// One RED run, one green run, QA and post-merge QA: none for the refusal.
	if reports != 1 || e.Job.TestRuns != 4 {
		t.Fatalf("RedReports %d, test runs %d", reports, e.Job.TestRuns)
	}
}

// After RestartWithInformation, a task whose latest RedReport is broken (here
// the restarted Spec declares a second test file the report also names) runs
// its test-first turn, which replaces the report, and reaches Done.
func TestDaemonLiveDerivedBrokenRedReportRecoversAfterRestart(t *testing.T) {
	d, _ := redRecoveryFixture(t, "restart")
	if _, err := call(t, d, "Admit", AdmitParams{ID: "red-restart", Title: "Make answer return 42", Content: "answer() must return 42", AuthorizingMessage: "message"}, "red-restart"); err != nil {
		t.Fatal(err)
	}
	e := awaitRedRecoverySettled(t, d, "red-restart")
	if e.Job.State != domain.Escalated || e.Job.EscalationKind != domain.HumanDecisionRequired {
		t.Fatalf("first pass ended %s (%s %s)", e.Job.State, e.Job.EscalationKind, e.Job.Reason)
	}
	// The second pass repeats the workflow; its attempts are authorized here.
	if _, err := call(t, d, "SetAttemptLimit", TaskControlParams{ID: "red-restart", AttemptLimit: 60, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, d, "RestartWithInformation", TaskControlParams{ID: "red-restart", Information: redRecoveryMarker, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	e = awaitRedRecoverySettled(t, d, "red-restart")
	failures := redRecoveryFailures(e)
	if e.Job.State != domain.Done {
		t.Fatalf("restarted task ended %s (%s %s): failures %v", e.Job.State, e.Job.EscalationKind, e.Job.Reason, failures)
	}
	if len(failures["test-first"]) != 0 {
		t.Fatalf("test-first refused after the restart: %v", failures["test-first"])
	}
	var latest map[string]any
	for _, a := range e.Job.Evidence {
		if a.Type == "RedReport" {
			latest = a.Fields
		}
	}
	if files, _ := liveEvidencePaths(latest["declared_files"]); len(files) != 2 {
		t.Fatalf("latest RedReport %v", latest)
	}
}

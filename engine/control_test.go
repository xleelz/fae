package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func apply(t *testing.T, j engine.Job, c engine.Control) engine.Job {
	t.Helper()
	out, ev, err := engine.ApplyControl(j, c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) < 1 || ev[len(ev)-1].Kind != "task-control" {
		t.Fatal("missing control event")
	}
	if err = engine.Validate(out); err != nil {
		t.Fatal(err)
	}
	return roundTrip(t, out)
}
func firstDispatch(t *testing.T, j engine.Job) (engine.Job, *engine.Dispatch) {
	t.Helper()
	for i := 0; i < 30; i++ {
		next, d, _, err := engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		j = next
		if d != nil {
			return j, d
		}
	}
	t.Fatal("no dispatch")
	return j, nil
}

func TestTaskControlsLegalSourcesAndTerminalRefusals(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name  string
		setup func(engine.Job) engine.Job
		c     engine.Control
	}{
		{"Escalate", nil, engine.Control{Method: "Escalate", Reason: "need owner"}},
		{"Block", func(j engine.Job) engine.Job { j.State = domain.State(j.Document.Start); return j }, engine.Control{Method: "Block", Reason: "missing dependency"}},
		{"Cancel", nil, engine.Control{Method: "Cancel", Reason: "not needed"}},
		{"Defer", nil, engine.Control{Method: "Defer", Until: now.Add(time.Hour)}},
		{"Undefer", func(j engine.Job) engine.Job { j.Deferred = true; return j }, engine.Control{Method: "Undefer"}},
		{"Reprioritize", nil, engine.Control{Method: "Reprioritize", Priority: 30}},
		{"SetAttemptLimit", nil, engine.Control{Method: "SetAttemptLimit", AttemptLimit: 50}},
		{"RestartWithInformation", nil, engine.Control{Method: "RestartWithInformation", Information: "new acceptance criterion"}},
		{"ResumeWithContext", nil, engine.Control{Method: "ResumeWithContext", Context: "use fixture"}},
		{"Resume", func(j engine.Job) engine.Job {
			j.State = domain.Escalated
			j.Reason = "AttemptCap"
			j.AttemptCapStage = j.Document.Start
			j.Attempts = 20
			j.TaskAttemptLimit = 21
			return j
		}, engine.Control{Method: "Resume"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j := newJob(t)
			if tc.setup != nil {
				j = tc.setup(j)
			}
			apply(t, j, tc.c)
			for _, terminal := range []domain.State{domain.Done, domain.Retired, domain.Cancelled} {
				j.State = terminal
				if _, _, err := engine.ApplyControl(j, tc.c, now); err == nil {
					t.Fatal("terminal accepted", terminal)
				}
			}
		})
	}
}

func TestTaskControlRefusals(t *testing.T) {
	for _, c := range []engine.Control{{Method: "Escalate"}, {Method: "Block", Reason: "reason"}, {Method: "Cancel"}, {Method: "Defer", Until: time.Now().Add(-time.Hour)}, {Method: "Undefer"}, {Method: "Reprioritize", Priority: 9}, {Method: "SetAttemptLimit", AttemptLimit: 0}, {Method: "RestartWithInformation"}, {Method: "ResumeWithContext"}, {Method: "Resume"}, {Method: "unknown"}} {
		t.Run(c.Method, func(t *testing.T) {
			if _, _, err := engine.ApplyControl(newJob(t), c, time.Now()); err == nil {
				t.Fatal("invalid control accepted")
			}
		})
	}
	j, d := firstDispatch(t, newJob(t))
	before, _ := json.Marshal(j)
	for _, method := range []string{"Cancel", "Defer", "Undefer", "RestartWithInformation", "Resume"} {
		if _, _, err := engine.ApplyControl(j, engine.Control{Method: method, Reason: "reason", Information: "info"}, time.Now()); err == nil {
			t.Fatal("in-flight accepted", method)
		}
	}
	after, _ := json.Marshal(j)
	if string(before) != string(after) || j.InFlight.ID != d.ID {
		t.Fatal("refusal mutated input")
	}
}

func TestTaskContextIsAppendOnlyAndFrozenPerDispatch(t *testing.T) {
	j := apply(t, newJob(t), engine.Control{Method: "RestartWithInformation", Information: "first"})
	j, d := firstDispatch(t, j)
	before, _ := json.Marshal(d)
	j = apply(t, j, engine.Control{Method: "ResumeWithContext", Context: "second"})
	after, _ := json.Marshal(j.InFlight)
	if string(before) != string(after) || strings.Contains(d.OperatorContext, "second") {
		t.Fatal("in-flight context changed")
	}
	if j.State != domain.State(d.Stage) || len(j.ContextEntries) != 2 {
		t.Fatal("context changed stage")
	}
	executor := engine.NewScripted(defaults.HappyScript())
	r, err := executor.Execute(context.Background(), *d)
	if err != nil {
		t.Fatal(err)
	}
	j, _, err = engine.Complete(j, d.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	j, next := firstDispatch(t, j)
	if !strings.Contains(next.OperatorContext, "first") || !strings.Contains(next.OperatorContext, "second") {
		t.Fatal("later brief dropped append")
	}
	j.ContextEntries[0].Text = "tampered"
	if engine.Validate(j) == nil {
		t.Fatal("tampered content accepted")
	}
}

func TestTaskDeferDeadlineAndPriorityGuard(t *testing.T) {
	j := apply(t, newJob(t), engine.Control{Method: "Defer"})
	next, d, events, err := engine.Prepare(j)
	if err != nil || d != nil || len(events) != 0 || next.State != domain.Pending {
		t.Fatal("indefinite defer ran")
	}
	j = apply(t, j, engine.Control{Method: "Undefer"})
	next, _, _, err = engine.Prepare(j)
	if err != nil || next.State == domain.Pending {
		t.Fatal("undefer not ready")
	}
	j = newJob(t)
	j.Deferred = true
	j.DeferredUntil = time.Now().Add(-time.Second)
	next, _, events, err = engine.Prepare(j)
	if err != nil || next.Deferred || next.State == domain.Pending || events[0].Kind != "undefer" {
		t.Fatal("deadline did not reenter")
	}
	for _, terminal := range []bool{false, true} {
		for _, flight := range []bool{false, true} {
			for _, allowed := range []bool{false, true} {
				if engine.CanChangeTask(terminal, flight, allowed) != (!terminal && (!flight || allowed)) {
					t.Fatal("guard")
				}
			}
		}
	}
}

func TestTaskAttemptLimitRaiseRequiresExplicitResume(t *testing.T) {
	j := newJob(t)
	j.State = domain.Escalated
	j.Reason = "AttemptCap"
	j.AttemptCapStage = j.Document.Start
	j.Attempts = j.Document.Limits.Attempts
	j = apply(t, j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: j.Attempts + 1})
	if j.State != domain.Escalated {
		t.Fatal("raise resumed")
	}
	j = apply(t, j, engine.Control{Method: "ResumeWithContext", Context: "additional attempt approved"})
	if j.State != domain.Escalated {
		t.Fatal("context resumed")
	}
	hash := j.WorkflowHash
	j = apply(t, j, engine.Control{Method: "Resume"})
	_, d := firstDispatch(t, j)
	if d.Attempt != j.Attempts+1 || hash != j.WorkflowHash {
		t.Fatal("budget not regranted or workflow changed")
	}
	j.State = domain.Escalated
	j.Reason = "OtherEscalation"
	if _, _, err := engine.ApplyControl(j, engine.Control{Method: "Resume"}, time.Now()); err == nil {
		t.Fatal("raise bypassed other escalation")
	}
	j.Reason = "AttemptCap"
	j.AttemptCapStage = j.Document.Start
	j.TaskAttemptLimit = j.Attempts
	if _, _, err := engine.ApplyControl(j, engine.Control{Method: "Resume"}, time.Now()); err == nil {
		t.Fatal("exhausted resume")
	}
}

func TestTaskBlockRemediationAndNoRoute(t *testing.T) {
	j := newJob(t)
	j.State = domain.State(j.Document.Start)
	j = apply(t, j, engine.Control{Method: "Block", Reason: "wait"})
	if j.State != domain.Blocked {
		t.Fatal("not blocked")
	}
	if _, _, err := engine.ApplyControl(j, engine.Control{Method: "Resume"}, time.Now()); err == nil {
		t.Fatal("missing remediation accepted")
	}
	j = newJob(t)
	for id, s := range j.Document.Stages {
		if _, ok := j.Document.Stages[s.Failure]; ok {
			j.State = domain.State(id)
			break
		}
	}
	from := j.State
	want := j.Document.Stages[string(from)].Failure
	if want == "" {
		t.Fatal("fixture no route")
	}
	j = apply(t, j, engine.Control{Method: "Block", Reason: "wait"})
	j = apply(t, j, engine.Control{Method: "Resume"})
	if string(j.State) != want {
		t.Fatal("remediation route lost")
	}
}

func TestTaskOperatorHoldCollectsAndRecoversInFlight(t *testing.T) {
	for _, method := range []string{"Escalate", "Block"} {
		t.Run(method, func(t *testing.T) {
			j, d := firstDispatch(t, newJob(t))
			j = apply(t, j, engine.Control{Method: method, Reason: "operator stop"})
			held := j.State
			r, err := engine.NewScripted(defaults.HappyScript()).Execute(context.Background(), *d)
			if err != nil {
				t.Fatal(err)
			}
			out, events, err := engine.Complete(j, d.ID, r)
			if err != nil {
				t.Fatal(err)
			}
			if out.State != held || !out.Parked || out.InFlight != nil || len(out.Provenance) != 1 || out.OperatorStage != "" {
				t.Fatal("hold lost or outcome discarded")
			}
			for _, e := range events {
				if e.Kind == "transition" {
					t.Fatal("held turn emitted false normal transition")
				}
			}
			if err = engine.Validate(out); err != nil {
				t.Fatal(err)
			}
			recovered, replay, err := engine.Recover(j, false, true)
			if err != nil || replay != nil || recovered.Reason != "UncertainDispatch" || recovered.State != held {
				t.Fatal("held crash replayed", err)
			}
			if _, _, err = engine.ResolveUncertain(recovered, d.ID, "redispatch", nil); err == nil {
				t.Fatal("held dispatch replayed")
			}
			out, _, err = engine.ResolveUncertain(recovered, d.ID, "escalate", nil)
			if err != nil || out.InFlight != nil || out.OperatorStage != "" {
				t.Fatal("uncertain hold not resolved", err)
			}
		})
	}
}

func TestTaskControlRestartEscalatedFirstStage(t *testing.T) {
	j := newJob(t)
	j.State = domain.Escalated
	j.Reason = "OperatorEscalate"
	j = apply(t, j, engine.Control{Method: "RestartWithInformation", Information: "fixed missing inputs"})
	if string(j.State) != j.Document.Start || j.Parked {
		t.Fatal("restart did not resume first stage")
	}
	if _, _, err := engine.ApplyControl(j, engine.Control{Method: "RestartWithInformation", Information: "x"}, time.Now()); err == nil {
		t.Fatal("stage restart accepted")
	}
}

func TestTaskRestartClearsPendingDeferral(t *testing.T) {
	j := apply(t, newJob(t), engine.Control{Method: "Defer"})
	j = apply(t, j, engine.Control{Method: "Escalate", Reason: "review"})
	j = apply(t, j, engine.Control{Method: "RestartWithInformation", Information: "proceed"})
	if j.Deferred || !j.DeferredUntil.IsZero() {
		t.Fatal("restart retained pending deferral")
	}
	firstDispatch(t, j)
}

// FAE-WF-021/-074, FAE-CLI-041: Escalated has exactly two out-edges, so a
// raise plus resume re-grants attempts from the first stage, keeping evidence.
func TestTaskAttemptResumeRestartsAtFirstStage(t *testing.T) {
	j, d := firstDispatch(t, newJob(t))
	r, err := engine.NewScripted(defaults.HappyScript()).Execute(context.Background(), *d)
	if err != nil {
		t.Fatal(err)
	}
	j, _, err = engine.Complete(j, d.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	j, _ = firstDispatch(t, j)
	j.InFlight = nil
	j.Attempts = j.AttemptLimit()
	stage := j.State
	j, _, _, err = engine.Prepare(j)
	if err != nil || j.Reason != "AttemptCap" || j.AttemptCapStage != string(stage) {
		t.Fatal("cap stage missing", err)
	}
	evidence := len(j.Evidence)
	j = apply(t, j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: j.Attempts + 1})
	out, events, err := engine.ApplyControl(j, engine.Control{Method: "Resume"}, time.Now())
	if err != nil || string(out.State) != out.Document.Start || len(out.Evidence) != evidence {
		t.Fatal("resume did not restart at the first stage", err, out.State)
	}
	if events[0].Kind != "transition" || events[0].From != domain.Escalated || string(events[0].To) != out.Document.Start || !workflow.CanTransition(out.Document, events[0].From, events[0].To) {
		t.Fatal("resume transition", events[0])
	}
	_, next := firstDispatch(t, out)
	if next.Attempt != j.Attempts+1 {
		t.Fatal("attempts not re-granted", next.Attempt)
	}
}

// attemptCapped is a task escalated AttemptCap with spent per-task budgets:
// test runs, Loop iterations, a loop-progress streak, governed requests and an
// approved scope.
func attemptCapped(t *testing.T) engine.Job {
	t.Helper()
	j := newJob(t)
	j.State, j.EscalationKind, j.Reason = domain.Escalated, domain.AttemptCap, string(domain.AttemptCap)
	j.AttemptCapStage = "implement"
	j.Attempts, j.TestRuns = j.Document.Limits.Attempts, 5
	j.Counts["implement"], j.Counts["implement-loop"], j.Counts["spec-loop"] = 6, 5, 1
	j.LoopProgress = map[string]engine.LoopProgress{"implement-loop": {Last: 2, Streak: 1}}
	j.RequestCounts = map[string]int{workflow.RequestScopeWiden: 3, workflow.RequestRedRework: 1}
	j.ApprovedScope = []string{"extra.go"}
	j.Failures["same-gate:run-tests:feedface"] = 2
	return roundTrip(t, j)
}

// carriedCounters is the counter state a restart from Escalated must keep.
func carriedCounters(j engine.Job) string {
	b, _ := json.Marshal(struct {
		Attempts      int
		TestRuns      int
		Counts        map[string]int
		Failures      map[string]int
		LoopProgress  map[string]engine.LoopProgress
		RequestCounts map[string]int
		ApprovedScope []string
	}{j.Attempts, j.TestRuns, j.Counts, j.Failures, j.LoopProgress, j.RequestCounts, j.ApprovedScope})
	return string(b)
}

// FAE-WF-074, FAE-OPS-010, FAE-CLI-041 (operator ruling: keep per-task
// limits): a raise plus resume of an AttemptCap escalation re-grants attempts
// and re-enters the first stage (FAE-WF-021). It refunds nothing: the test
// runs, Loop counts, loop-progress streaks and governed-request counts carry
// over, and the audited control records the carried values. A task whose
// other budgets remain then reaches Done within the per-task test-run cap.
func TestAttemptCapResumeRegrantsAttemptsOnlyToDone(t *testing.T) {
	script := defaults.HappyScript()
	block := oracle(false, finding("Production", true))
	script["oracle"] = []engine.ScriptStep{block, script["oracle"][0]}
	for k, steps := range script {
		for len(steps) < 8 {
			steps = append(steps, steps[len(steps)-1])
		}
		script[k] = steps
	}
	executor := engine.NewScripted(script)
	// One attempt: the oracle's production rework re-enters implement, the
	// first repeated execution, and exhausts it.
	j := apply(t, newJob(t), engine.Control{Method: "SetAttemptLimit", AttemptLimit: 1})
	j = drive(t, j, executor, false)
	// Every pass from the first stage enters spec-loop, red-loop and
	// implement-loop once; the rework entered implement-loop again.
	if !j.Escalated(domain.AttemptCap) || j.AttemptCapStage != "implement" || j.TestRuns != 2 || j.Counts["implement-loop"] != 2 {
		t.Fatalf("fixture: %s %s at %s, test runs %d, loops %v", j.State, j.EscalationKind, j.AttemptCapStage, j.TestRuns, j.Counts)
	}
	before := carriedCounters(j)
	j = apply(t, j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: 100})
	out, events, err := engine.ApplyControl(j, engine.Control{Method: "Resume", Reason: "raised after review"}, time.Now())
	if err != nil || string(out.State) != out.Document.Start || !out.NewAttempt || len(out.Evidence) != len(j.Evidence) {
		t.Fatal("resume", err, out.State)
	}
	if after := carriedCounters(out); after != before {
		t.Fatalf("resume refunded a budget:\nbefore %s\nafter  %s", before, after)
	}
	audit := events[len(events)-1]
	want := fmt.Sprintf(`Resume: raised after review; restart from %s refunds nothing; carries counters {"attempts":%d,"attempt_limit":100,"test_runs":2,"loops":{"implement-loop":2,"red-loop":1,"spec-loop":1}}`, out.Document.Start, j.Attempts)
	if audit.Kind != "task-control" || !strings.HasPrefix(audit.Cause, want) {
		t.Fatalf("carried counters not audited:\n got %s\nwant %s...", audit.Cause, want)
	}
	if _, d := firstDispatch(t, out); d.Stage != out.Document.Start || d.Attempt != j.Attempts+1 {
		t.Fatal("re-granted dispatch", d.Stage, d.Attempt)
	}
	done := drive(t, roundTrip(t, out), executor, false)
	// The second pass re-runs RED, GREEN, QA and post-merge QA (2 + 4 test
	// runs) and enters each loop once more, counted on the carried values.
	if done.State != domain.Done || done.TestRuns != 6 || done.Counts["implement-loop"] != 3 || done.Counts["spec-loop"] != 2 || done.Counts["red-loop"] != 2 {
		t.Fatalf("re-granted task: %s %s %q test runs %d loops %v", done.State, done.EscalationKind, done.EscalationDetail, done.TestRuns, done.Counts)
	}
}

// FAE-WF-074: raising the limit never bypasses another escalation kind. Only
// AttemptCap resumes, and its resume refunds no other budget, so TestRunCap,
// LoopCap and every other kind stay final until the operator retires or
// restarts the task with information.
func TestResumeRefusesEveryOtherEscalationKind(t *testing.T) {
	for _, kind := range domain.EscalationKinds {
		if kind == domain.AttemptCap {
			continue
		}
		t.Run(string(kind), func(t *testing.T) {
			j := newJob(t)
			j.State, j.EscalationKind, j.Reason = domain.Escalated, kind, string(kind)
			j.Attempts, j.TestRuns = j.Document.Limits.Attempts, j.Document.Limits.TestRuns
			j.Counts["implement-loop"], j.RequestCounts = 8, map[string]int{workflow.RequestScopeWiden: 3}
			j.LoopProgress = map[string]engine.LoopProgress{"implement-loop": {Last: 2, Streak: 1}}
			j.Failures["same-gate:run-tests:feedface"] = 2
			j = apply(t, j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: j.Attempts + 10})
			before, _ := json.Marshal(j)
			counters := carriedCounters(j)
			out, events, err := engine.ApplyControl(j, engine.Control{Method: "Resume"}, time.Now())
			if err == nil {
				t.Fatal("raise bypassed", kind)
			}
			// The refusal returns the task with its counters, state and
			// escalation unchanged and records nothing ...
			if got := carriedCounters(out); got != counters || out.State != domain.Escalated || out.EscalationKind != kind || out.AttemptLimit() != j.AttemptLimit() || len(events) != 0 {
				t.Fatalf("refused resume changed counters:\nbefore %s\nafter  %s", counters, got)
			}
			// ... and leaves the caller's task untouched.
			if after, _ := json.Marshal(j); string(before) != string(after) {
				t.Fatal("refused resume mutated the task in place")
			}
		})
	}
}

// FAE-WF-074, FAE-WF-078, FAE-OPS-010: Resume of an AttemptCap escalation and
// restart with information take the same Escalated -> first-stage edge and
// agree: neither refunds a per-task budget. Request counts, approved scope,
// Loop counts, progress streaks, test runs and stage failure counters carry
// over, and both audited controls record the carried values.
func TestAttemptCapResumeAndRestartCarryEveryCounter(t *testing.T) {
	j := attemptCapped(t)
	if _, _, err := engine.ApplyControl(j, engine.Control{Method: "Resume"}, time.Now()); err == nil {
		t.Fatal("resume without a raise")
	}
	if _, _, err := engine.ApplyControl(j, engine.Control{Method: "RestartWithInformation", Information: "more"}, time.Now()); err == nil {
		t.Fatal("restart without a raise")
	}
	j = apply(t, j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: j.Attempts + 5})
	before := carriedCounters(j)
	carried := `refunds nothing; carries counters {"attempts":20,"attempt_limit":25,"test_runs":5,"loops":{"implement-loop":5,"spec-loop":1},"loop_progress":{"implement-loop":{"last":2,"streak":1}},"requests":{"red-rework":1,"scope-widen":3}}`
	for _, c := range []engine.Control{{Method: "Resume", Reason: "raised after review"}, {Method: "RestartWithInformation", Information: "acceptance clarified"}} {
		out := apply(t, j, c)
		if string(out.State) != out.Document.Start || !out.NewAttempt || out.EscalationKind != "" {
			t.Fatal(c.Method, "did not restart at the first stage", out.State)
		}
		if after := carriedCounters(out); after != before {
			t.Fatalf("%s refunded a budget:\nbefore %s\nafter  %s", c.Method, before, after)
		}
		if cause := out.History[len(out.History)-1].Cause; !strings.HasPrefix(cause, c.Method+":") || !strings.HasSuffix(cause, carried) {
			t.Fatalf("%s audit: %s", c.Method, cause)
		}
		if _, d := firstDispatch(t, out); d.Attempt != j.Attempts+1 || !slices.Equal(d.Scope, []string{"extra.go"}) {
			t.Fatal(c.Method, "re-granted dispatch", d.Attempt, d.Scope)
		}
	}
}

// FAE-OPS-010, FAE-WF-074: the budgets a restart carries, as the daemon's
// Resume and restart replies state them: each count with its limit, the
// attempts against the raised limit.
func TestCarriedBudgetsNameUsedAndLimit(t *testing.T) {
	j := apply(t, attemptCapped(t), engine.Control{Method: "SetAttemptLimit", AttemptLimit: 25})
	out := apply(t, j, engine.Control{Method: "Resume", Reason: "raised"})
	b, _ := json.Marshal(engine.CarriedBudgets(out))
	want := `{"attempts":{"used":20,"limit":25},"test_runs":{"used":5,"limit":8},"loops":{"implement-loop":{"used":5,"limit":8},"integration-retry-loop":{"used":0,"limit":1},"red-loop":{"used":0,"limit":8},"reverify-loop":{"used":0,"limit":3},"spec-loop":{"used":1,"limit":3}},"requests":{"red-rework":{"used":1,"limit":3},"scope-widen":{"used":3,"limit":3}}}`
	if string(b) != want {
		t.Fatalf("carried budgets\n got %s\nwant %s", b, want)
	}
}

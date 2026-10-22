package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
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

func TestTaskAttemptResumeRetainsInterruptedStage(t *testing.T) {
	j, d := firstDispatch(t, newJob(t))
	r, err := engine.NewScripted(defaults.HappyScript()).Execute(context.Background(), *d)
	if err != nil {
		t.Fatal(err)
	}
	j, _, err = engine.Complete(j, d.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	j, second := firstDispatch(t, j)
	j.InFlight = nil
	j.Attempts = j.AttemptLimit()
	stage := j.State
	j, _, _, err = engine.Prepare(j)
	if err != nil || j.Reason != "AttemptCap" || j.AttemptCapStage != string(stage) {
		t.Fatal("cap stage missing", err)
	}
	j = apply(t, j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: j.Attempts + 1})
	j = apply(t, j, engine.Control{Method: "Resume"})
	if j.State != stage || j.State == domain.State(j.Document.Start) {
		t.Fatal("resume replayed completed first stage")
	}
	_, next := firstDispatch(t, j)
	if next.Stage != second.Stage {
		t.Fatal("wrong resumed stage")
	}
}

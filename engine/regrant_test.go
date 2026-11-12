package engine_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// gatedOracle serves the shipped default's happy script, except that final
// judgement blocks with a valid production finding until approve is set: the
// re-audit's probe of raise-and-resume (WF-074).
type gatedOracle struct {
	happy   *engine.Scripted
	approve bool
}

func newGatedOracle() *gatedOracle {
	script := defaults.HappyScript()
	for k, steps := range script {
		for len(steps) < 64 {
			steps = append(steps, steps[len(steps)-1])
		}
		script[k] = steps
	}
	return &gatedOracle{happy: engine.NewScripted(script)}
}

func (g *gatedOracle) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	if d.Stage != "oracle" {
		return g.happy.Execute(ctx, d)
	}
	step := oracle(false, finding("Production", true))
	if g.approve {
		step = oracle(true)
	}
	return engine.Result{DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, Output: step.Output, ProducedAt: time.Now().UTC()}, nil
}

// driveBudgeted drives a task until it stops, calling before ahead of every
// decision, and fails the moment any per-task budget is exceeded. The
// operator controls between drives round-trip the task (apply).
func driveBudgeted(t *testing.T, j engine.Job, ex engine.Executor, before func(engine.Job) engine.Job) engine.Job {
	t.Helper()
	for i := 0; i < 2000; i++ {
		if j.State.Terminal() || j.State == domain.Escalated {
			return j
		}
		if j.Parked {
			t.Fatalf("parked: %s", j.Reason)
		}
		if before != nil {
			j = before(j)
		}
		next, d, _, err := engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		j = next
		if d != nil {
			r, err := ex.Execute(context.Background(), *d)
			if err != nil {
				t.Fatal(err)
			}
			if j, _, err = engine.Complete(j, d.ID, r); err != nil {
				t.Fatal(err)
			}
		}
		budgetsHeld(t, j)
	}
	t.Fatal("unbounded")
	return j
}

// budgetsHeld checks the per-task caps of Appendix A and every Loop's cap.
func budgetsHeld(t *testing.T, j engine.Job) {
	t.Helper()
	if j.TestRuns > j.Document.Limits.TestRuns {
		t.Fatalf("test runs %d exceed the per-task cap %d", j.TestRuns, j.Document.Limits.TestRuns)
	}
	if n := j.RequestCounts[workflow.RequestScopeWiden]; n > j.Document.Limits.ScopeWidens {
		t.Fatalf("scope widens %d exceed the per-task cap %d", n, j.Document.Limits.ScopeWidens)
	}
	for id, s := range j.Document.Stages {
		if s.Primitive == workflow.Loop && j.Counts[id] > s.MaxIterations {
			t.Fatalf("%s ran %d iterations past its cap %d", id, j.Counts[id], s.MaxIterations)
		}
	}
}

func raise(t *testing.T, j engine.Job, limit int) engine.Job {
	t.Helper()
	return apply(t, j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: limit})
}

// FAE-WF-074, Appendix A (operator ruling: keep per-task limits). The
// re-audit's probe scenarios: a task whose test-run budget is spent when its
// AttemptCap fires, or whose operator lowers the limit once that budget is
// spent, or that is raised and resumed repeatedly, or restarted with
// information, never runs more than 8 test runs, and ends TestRunCap, which
// no raise resumes. Before the fix, Resume refunded the spent budgets and the
// task finished with 12 (and, repeated, 26) test runs.
func TestAttemptCapRegrantNeverRefundsSpentBudgets(t *testing.T) {
	testRunCap := func(t *testing.T, j engine.Job) {
		t.Helper()
		if !j.Escalated(domain.TestRunCap) || j.TestRuns != j.Document.Limits.TestRuns {
			t.Fatalf("ended %s %s %q with %d test runs", j.State, j.EscalationKind, j.EscalationDetail, j.TestRuns)
		}
		raised := raise(t, j, j.Attempts+100)
		if _, _, err := engine.ApplyControl(raised, engine.Control{Method: "Resume"}, time.Now()); err == nil {
			t.Fatal("a raise resumed TestRunCap")
		}
	}
	t.Run("refund", func(t *testing.T) {
		t.Parallel()
		g := newGatedOracle()
		j := driveBudgeted(t, raise(t, newJob(t), 36), g, nil)
		if !j.Escalated(domain.AttemptCap) || j.TestRuns != j.Document.Limits.TestRuns {
			t.Fatalf("fixture: %s %s with %d test runs", j.State, j.EscalationKind, j.TestRuns)
		}
		runs, loops := j.TestRuns, j.Counts["implement-loop"]
		j = apply(t, raise(t, j, 500), engine.Control{Method: "Resume", Reason: "raised"})
		if j.TestRuns != runs || j.Counts["implement-loop"] != loops {
			t.Fatal("resume refunded", j.TestRuns, j.Counts["implement-loop"])
		}
		g.approve = true
		j = driveBudgeted(t, j, g, nil)
		testRunCap(t, j)
		if j.Attempts <= 36 {
			t.Fatal("the re-grant was not used", j.Attempts)
		}
	})
	t.Run("lower", func(t *testing.T) {
		t.Parallel()
		g := newGatedOracle()
		lowered := false
		j := driveBudgeted(t, raise(t, newJob(t), 500), g, func(j engine.Job) engine.Job {
			if !lowered && j.TestRuns == j.Document.Limits.TestRuns && j.InFlight == nil {
				lowered = true
				return raise(t, j, j.Attempts)
			}
			return j
		})
		if !lowered || !j.Escalated(domain.AttemptCap) {
			t.Fatal("fixture", lowered, j.State, j.EscalationKind)
		}
		g.approve = true
		j = driveBudgeted(t, apply(t, raise(t, j, 500), engine.Control{Method: "Resume", Reason: "raised"}), g, nil)
		testRunCap(t, j)
	})
	t.Run("repeat", func(t *testing.T) {
		t.Parallel()
		g := newGatedOracle()
		j := raise(t, newJob(t), 20)
		passes := 0
		for ; passes < 8; passes++ {
			j = driveBudgeted(t, j, g, nil)
			if !j.Escalated(domain.AttemptCap) {
				break
			}
			j = apply(t, raise(t, j, j.Attempts+20), engine.Control{Method: "Resume", Reason: "raised"})
		}
		// The second pass spends the last test run with the raised attempts:
		// both bounds hold at that boundary, and it is TestRunCap.
		if passes != 1 || j.Attempts != j.AttemptLimit() {
			t.Fatalf("passes %d, attempts %d of %d", passes+1, j.Attempts, j.AttemptLimit())
		}
		testRunCap(t, j)
	})
	t.Run("restart", func(t *testing.T) {
		t.Parallel()
		g := newGatedOracle()
		j := driveBudgeted(t, raise(t, newJob(t), 36), g, nil)
		if !j.Escalated(domain.AttemptCap) || j.TestRuns != j.Document.Limits.TestRuns {
			t.Fatalf("fixture: %s %s with %d test runs", j.State, j.EscalationKind, j.TestRuns)
		}
		g.approve = true
		j = driveBudgeted(t, apply(t, raise(t, j, 500), engine.Control{Method: "RestartWithInformation", Information: "more context"}), g, nil)
		testRunCap(t, j)
	})
}

// FAE-WF-074: where the test-run budget and the attempt limit are both spent
// at one attempt boundary, the task escalates TestRunCap, which a raise never
// resumes, not AttemptCap. Either bound alone keeps its own kind.
func TestTestRunCapPrecedesAttemptCapAtOneBoundary(t *testing.T) {
	executor := engine.NewScripted(defaults.HappyScript())
	j := newJob(t)
	for i := 0; i < 50 && !(j.State == "run-red" && j.InFlight == nil); i++ {
		next, d, _, err := engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		j = next
		if d != nil {
			r, err := executor.Execute(context.Background(), *d)
			if err != nil {
				t.Fatal(err)
			}
			if j, _, err = engine.Complete(j, d.ID, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	if j.State != "run-red" || !j.Document.Stages["run-red"].TestRun {
		t.Fatal("fixture did not reach the RED run", j.State)
	}
	// A re-granted attempt boundary at a test-run stage.
	j.NewAttempt = true
	for _, tc := range []struct {
		attempts, runs int
		kind           domain.EscalationKind
	}{
		{j.AttemptLimit(), j.Document.Limits.TestRuns, domain.TestRunCap},
		{j.AttemptLimit(), j.Document.Limits.TestRuns - 1, domain.AttemptCap},
		{j.AttemptLimit() - 1, j.Document.Limits.TestRuns, domain.TestRunCap},
	} {
		k := roundTrip(t, j)
		k.Attempts, k.TestRuns = tc.attempts, tc.runs
		out, d, _, err := engine.Prepare(k)
		if err != nil || d != nil || !out.Escalated(tc.kind) {
			t.Fatalf("attempts %d, test runs %d: %s %s, dispatch %v, %v", tc.attempts, tc.runs, out.State, out.EscalationKind, d != nil, err)
		}
	}
}

// FAE-WF-074: a Loop already at its cap stays there across a raise and
// resume. The pass from the first stage enters implement-loop on its way to
// implement, and that entry escalates LoopCap instead of iterating again.
func TestAttemptCapResumeKeepsALoopAtItsCap(t *testing.T) {
	j := attemptCapped(t)
	j.Counts["implement-loop"] = j.Document.Stages["implement-loop"].MaxIterations
	j = apply(t, raise(t, j, j.Attempts+100), engine.Control{Method: "Resume", Reason: "raised"})
	if j.Counts["implement-loop"] != 8 {
		t.Fatal("resume refunded the loop", j.Counts["implement-loop"])
	}
	j = driveBudgeted(t, j, newGatedOracle(), nil)
	if !j.Escalated(domain.LoopCap) || !strings.HasPrefix(j.EscalationDetail, "implement-loop exhausted 8 iterations") || j.Counts["implement"] != 6 || j.Counts["implement-loop"] != 8 {
		t.Fatalf("%s %s %q implement %d loop %d", j.State, j.EscalationKind, j.EscalationDetail, j.Counts["implement"], j.Counts["implement-loop"])
	}
}

// FAE-WF-074, FAE-WF-078, Appendix A: scope-widen requests are capped per
// task. Two approved widens, an AttemptCap, a raise and resume: one more widen
// is approved, the fourth exhausts the cap of 3 and escalates
// ScopeWidenRejected; the approved scope carries throughout.
func TestScopeWidenCapHoldsAcrossAttemptCapResume(t *testing.T) {
	doc := requestWorkflow()
	doc.Limits.ScopeWidens = 3
	j, err := engine.New("task", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	widenTo := func(file string) string {
		return `{"request":{"kind":"scope-widen","files":["` + file + `"],"reason":"the fix touches ` + file + `"}}`
	}
	exec := engine.NewScripted(map[string][]engine.ScriptStep{
		"work":  steps(widenTo("a.go"), widenTo("b.go"), widenTo("c.go"), widenTo("d.go"), `{}`),
		"judge": steps(`{"approved":true}`, `{"approved":true}`, `{"approved":true}`, `{"approved":true}`),
	})
	// work, judge, work, judge: the third work turn is the fourth attempt.
	j = driveBudgeted(t, raise(t, j, 3), exec, nil)
	if !j.Escalated(domain.AttemptCap) || j.RequestCounts[workflow.RequestScopeWiden] != 2 || !slices.Equal(j.ApprovedScope, []string{"a.go", "b.go"}) {
		t.Fatal("fixture", j.State, j.EscalationKind, j.RequestCounts, j.ApprovedScope)
	}
	j = apply(t, raise(t, j, 20), engine.Control{Method: "Resume", Reason: "raised"})
	if j.RequestCounts[workflow.RequestScopeWiden] != 2 || !strings.Contains(j.History[len(j.History)-1].Cause, `"requests":{"scope-widen":2}`) {
		t.Fatal("resume refunded the request cap", j.RequestCounts, j.History[len(j.History)-1].Cause)
	}
	j = driveBudgeted(t, j, exec, nil)
	if !j.Escalated(domain.ScopeWidenRejected) || !strings.Contains(j.EscalationDetail, "scope-widen cap 3 exhausted") || j.RequestCounts[workflow.RequestScopeWiden] != 3 || !slices.Equal(j.ApprovedScope, []string{"a.go", "b.go", "c.go"}) {
		t.Fatal("widen cap across resume", j.State, j.EscalationKind, j.EscalationDetail, j.RequestCounts, j.ApprovedScope)
	}
}

// FAE-OPS-010, FAE-WF-074 (operator ruling: keep per-task limits; Resume does
// not refuse a task whose other budget is spent): the escalation a resumed
// task later reaches explains itself. TestRunCap and LoopCap name the count
// used, the limit, and that the count carries for the life of the task.
func TestCarriedBudgetEscalationsNameTheirCounts(t *testing.T) {
	t.Run("TestRunCap", func(t *testing.T) {
		j := attemptCapped(t)
		j.TestRuns = j.Document.Limits.TestRuns
		j = apply(t, raise(t, j, j.Attempts+100), engine.Control{Method: "Resume", Reason: "raised"})
		j = driveBudgeted(t, j, newGatedOracle(), nil)
		want := "run-red needs a test run: 8 of 8 test runs used; the test-run count carries for the life of the task, across resume and restart"
		if !j.Escalated(domain.TestRunCap) || j.EscalationDetail != want || j.Reason != "TestRunCap: "+want {
			t.Fatalf("%s %s\n got %q\nwant %q", j.State, j.EscalationKind, j.EscalationDetail, want)
		}
	})
	t.Run("LoopCap", func(t *testing.T) {
		j := attemptCapped(t)
		j.Counts["spec-loop"] = j.Document.Stages["spec-loop"].MaxIterations
		j = apply(t, raise(t, j, j.Attempts+100), engine.Control{Method: "Resume", Reason: "raised"})
		j = driveBudgeted(t, j, newGatedOracle(), nil)
		want := "spec-loop exhausted 3 iterations: 3 of 3 used; the iteration count carries for the life of the task, across resume and restart"
		if !j.Escalated(domain.LoopCap) || j.EscalationDetail != want {
			t.Fatalf("%s %s\n got %q\nwant %q", j.State, j.EscalationKind, j.EscalationDetail, want)
		}
	})
}

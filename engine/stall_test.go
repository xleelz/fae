package engine_test

import (
	"encoding/json"
	"strings"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"testing"
	"time"
)

func TestStallRecoveryDurableBoundAndFreshSession(t *testing.T) {
	j, _ := engine.NewLive("stalls", 1, defaults.Workflow(), "tree")
	previous := ""
	for n := 1; n <= 3; n++ {
		var d *engine.Dispatch
		var e error
		for d == nil {
			j, d, _, e = engine.Prepare(j)
			if e != nil || j.Parked || j.State == domain.Escalated {
				t.Fatalf("prepare: %v %+v", e, j)
			}
		}
		if d.Sessions[0].ID == previous {
			t.Fatal("session reused")
		}
		previous = d.Sessions[0].ID
		r := engine.Result{DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC()}
		r.ExecutionKind = "live"
		r.Class = "stall-retry"
		j, _, e = engine.Complete(j, d.ID, r)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := json.Marshal(j)
		var restored engine.Job
		if e = json.Unmarshal(b, &restored); e != nil {
			t.Fatal(e)
		}
		j = restored
		// The counter is durable within the turn; the third stall's
		// escalation ends the turn and clears it.
		want := n
		if n == 3 {
			want = 0
		}
		if j.Failures["stall-recovery"] != want || j.InFlight != nil {
			t.Fatal("lost durable recovery counter", n, j.Failures["stall-recovery"])
		}
		if n < 3 && j.State == domain.Escalated {
			t.Fatal("early escalation")
		}
	}
	if j.State != domain.Escalated || j.EscalationKind != domain.StallExhausted || !strings.HasPrefix(j.Reason, "StallExhausted: intake stalled 3 times") {
		t.Fatalf("%s %s", j.State, j.Reason)
	}
	if j.Failures["dispatch-recovery"] != 0 {
		t.Fatal("mixed recovery classes")
	}
}

// FAE-RT-005, FAE-ROB-017: the stall-retry bound (2) is per stalled turn. A
// turn that ends restarts the durable counter, so a later turn that stalls is
// retried again: three turns each stalling twice are all retried and the task
// finishes. One turn stalling a third time escalates StallExhausted, which
// ends that turn too: the escalated row keeps no stall count.
func TestStallRetryBoundIsPerStalledTurn(t *testing.T) {
	script := defaults.HappyScript()
	for k, steps := range script {
		for len(steps) < 4 {
			steps = append(steps, steps[len(steps)-1])
		}
		script[k] = steps
	}
	run := func(stalls map[string]int) engine.Job {
		j, err := engine.NewLive("stalls", 1, defaults.Workflow(), "tree")
		if err != nil {
			t.Fatal(err)
		}
		return liveDrive(t, j, engine.NewScripted(script), func(d *engine.Dispatch, r *engine.Result) {
			if d.Config.ChangeProducer {
				r.NewTree = d.Tree + "+" + d.Stage
			}
			if stalls[d.Stage] > 0 {
				stalls[d.Stage]--
				*r = engine.Result{DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), ExecutionKind: "live", Class: "stall-retry"}
			}
		})
	}
	retries := func(j engine.Job) (n int) {
		for _, e := range j.History {
			if e.Kind == "stall-retry" {
				n++
			}
		}
		return n
	}
	done := run(map[string]int{"spec": 2, "plan": 2, "implement": 2})
	if done.State != domain.Done || retries(done) != 6 || done.Failures[engine.StallRecoveryKey] != 0 {
		t.Fatalf("stalls in three turns: %s %s, %d retries, counter %d", done.State, done.Reason, retries(done), done.Failures[engine.StallRecoveryKey])
	}
	exhausted := run(map[string]int{"spec": 2, "plan": 3})
	if !exhausted.Escalated(domain.StallExhausted) || exhausted.State != domain.Escalated || retries(exhausted) != 4 || exhausted.Failures[engine.StallRecoveryKey] != 0 || exhausted.Counts["plan"] != 3 {
		t.Fatalf("one turn stalling three times: %s %s, %d retries, counter %d", exhausted.State, exhausted.Reason, retries(exhausted), exhausted.Failures[engine.StallRecoveryKey])
	}
}

// FAE-RT-005: a turn that ends in a recorded failure has ended too; the
// stage's next dispatch is a new turn with a fresh stall-retry bound.
func TestStallRetryBoundRestartsAfterAFailedTurn(t *testing.T) {
	script := defaults.HappyScript()
	for k, steps := range script {
		for len(steps) < 8 {
			steps = append(steps, steps[len(steps)-1])
		}
		script[k] = steps
	}
	j, err := engine.NewLive("stalls", 1, defaults.Workflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	// plan: stall, stall, malformed report, stall, stall, success.
	outcomes := []string{"stall-retry", "stall-retry", engine.ClassMalformedOutput, "stall-retry", "stall-retry"}
	j = liveDrive(t, j, engine.NewScripted(script), func(d *engine.Dispatch, r *engine.Result) {
		if d.Config.ChangeProducer {
			r.NewTree = d.Tree + "+" + d.Stage
		}
		if d.Stage == "plan" && len(outcomes) > 0 {
			*r = engine.Result{DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), ExecutionKind: "live", Class: outcomes[0], Detail: "fixture"}
			outcomes = outcomes[1:]
		}
	})
	if j.State != domain.Done || j.Counts["plan"] != 6 {
		t.Fatalf("%s %s plan visits %d", j.State, j.Reason, j.Counts["plan"])
	}
}

// stallingLive drives a live default-workflow task on the happy script, each
// stage repeating its last step. A dispatch at a stage with stalls left
// returns a stall retry instead; a test changes the map between passes.
func stallingLive(t *testing.T, j engine.Job, stalls map[string]int) engine.Job {
	t.Helper()
	script := defaults.HappyScript()
	for k, steps := range script {
		for len(steps) < 8 {
			steps = append(steps, steps[len(steps)-1])
		}
		script[k] = steps
	}
	return liveDrive(t, j, engine.NewScripted(script), func(d *engine.Dispatch, r *engine.Result) {
		if d.Config.ChangeProducer {
			r.NewTree = d.Tree + "+" + d.Stage
		}
		if stalls[d.Stage] > 0 {
			stalls[d.Stage]--
			*r = engine.Result{DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), ExecutionKind: "live", Class: "stall-retry"}
		}
	})
}

func stallRetries(j engine.Job, from int) (n int) {
	for _, e := range j.History[from:] {
		if e.Kind == "stall-retry" {
			n++
		}
	}
	return n
}

// FAE-RT-005, FAE-ROB-017: an escalation ends the stalled turn too, so the
// bound restarts with the next turn. A task escalated StallExhausted and
// restarted with information gets its 2 retries on the next turn that
// stalls; the escalation's detail names the stalls it counted.
func TestStallExhaustedRestartGetsFreshRetries(t *testing.T) {
	j, err := engine.NewLive("stalls", 1, defaults.Workflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	// Two passes repeat every stage; attempts must not bound this test.
	j = apply(t, j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: 100})
	stalls := map[string]int{"plan": 3}
	j = stallingLive(t, j, stalls)
	if !j.Escalated(domain.StallExhausted) || stallRetries(j, 0) != 2 {
		t.Fatalf("fixture: %s %q, %d retries", j.State, j.Reason, stallRetries(j, 0))
	}
	exhausted := j
	j = apply(t, j, engine.Control{Method: "RestartWithInformation", Information: "plan in smaller steps"})
	restarted := len(j.History)
	// The restart's first turn, at the start stage, stalls twice.
	stalls["intake"] = 2
	j = stallingLive(t, j, stalls)
	if j.State != domain.Done || stallRetries(j, restarted) != 2 || j.Failures[engine.StallRecoveryKey] != 0 {
		t.Fatalf("restarted pass: %s %q, %d retries, counter %d", j.State, j.Reason, stallRetries(j, restarted), j.Failures[engine.StallRecoveryKey])
	}
	// The escalated row holds no stall count; its detail names the count.
	if exhausted.Failures[engine.StallRecoveryKey] != 0 {
		t.Fatalf("escalated row kept stall counter %d", exhausted.Failures[engine.StallRecoveryKey])
	}
	if want := "plan stalled 3 times in one turn: 2 of 2 retries used"; !strings.HasPrefix(exhausted.EscalationDetail, want) {
		t.Fatalf("StallExhausted detail %q, want prefix %q", exhausted.EscalationDetail, want)
	}
}

// FAE-RT-005, FAE-WF-074: the attempt limit can stop a stalled turn's retry.
// The AttemptCap escalation ends that turn, so after a raise and Resume the
// next stalled turn again gets both retries; the stall counter is a per-turn
// bound, not a per-task budget, and its clearing refunds nothing.
func TestAttemptCapMidStallResumeGetsFreshRetries(t *testing.T) {
	j, err := engine.NewLive("stalls", 1, defaults.Workflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	// One attempt: intake charges it, and spec's retry after one stall is a
	// repeated execution the limit refuses.
	j = apply(t, j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: 1})
	stalls := map[string]int{"spec": 1}
	j = stallingLive(t, j, stalls)
	if !j.Escalated(domain.AttemptCap) || j.AttemptCapStage != "spec" || stallRetries(j, 0) != 1 {
		t.Fatalf("fixture: %s %q at %s, %d retries", j.State, j.Reason, j.AttemptCapStage, stallRetries(j, 0))
	}
	capped := j.Failures[engine.StallRecoveryKey]
	j = apply(t, j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: 100})
	j = apply(t, j, engine.Control{Method: "Resume", Reason: "raised"})
	resumed := len(j.History)
	// The resumed pass's first turn, at the start stage, stalls twice.
	stalls["intake"] = 2
	j = stallingLive(t, j, stalls)
	if j.State != domain.Done || stallRetries(j, resumed) != 2 || j.Failures[engine.StallRecoveryKey] != 0 {
		t.Fatalf("resumed pass: %s %q, %d retries, counter %d", j.State, j.Reason, stallRetries(j, resumed), j.Failures[engine.StallRecoveryKey])
	}
	if capped != 0 {
		t.Fatalf("AttemptCap row kept stall counter %d", capped)
	}
}

// FAE-RT-005, FAE-OPS-010: an operator hold ends the stalled turn. A retry
// in flight when the operator escalates is collected under the hold and never
// retried, so the held row keeps no stall count and the restart's first turn
// gets both retries.
func TestHeldStallRetryEndsTheTurn(t *testing.T) {
	j, err := engine.NewLive("stalls", 1, defaults.Workflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	j = apply(t, j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: 100})
	stall := func(j engine.Job, d *engine.Dispatch) engine.Job {
		t.Helper()
		r := engine.Result{DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), ExecutionKind: "live", Class: "stall-retry"}
		out, _, err := engine.Complete(j, d.ID, r)
		if err != nil {
			t.Fatal(err)
		}
		return roundTrip(t, out)
	}
	j, d := firstDispatch(t, j)
	j = stall(j, d)
	j, d = firstDispatch(t, j)
	if d.Stage != "intake" || j.Failures[engine.StallRecoveryKey] != 1 {
		t.Fatalf("fixture: retry at %s, counter %d", d.Stage, j.Failures[engine.StallRecoveryKey])
	}
	j = apply(t, j, engine.Control{Method: "Escalate", Reason: "operator look"})
	j = stall(j, d)
	if j.State != domain.Escalated || j.InFlight != nil || j.Failures[engine.StallRecoveryKey] != 0 {
		t.Fatalf("held stall: %s in flight %v, counter %d", j.State, j.InFlight != nil, j.Failures[engine.StallRecoveryKey])
	}
	j = apply(t, j, engine.Control{Method: "RestartWithInformation", Information: "carry on"})
	restarted := len(j.History)
	j = stallingLive(t, j, map[string]int{"intake": 2})
	if j.State != domain.Done || stallRetries(j, restarted) != 2 {
		t.Fatalf("restarted pass: %s %q, %d retries", j.State, j.Reason, stallRetries(j, restarted))
	}
}

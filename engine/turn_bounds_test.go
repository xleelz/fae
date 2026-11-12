package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// turnBoundsTask is a live default-workflow task whose roles have four
// routes, so failover rotations have somewhere to go. The route selector
// takes the first binding a failover has not made ineligible.
type turnBoundsTask struct {
	t       *testing.T
	j       engine.Job
	healthy map[string]bool
	options engine.Options
}

func newTurnBoundsTask(t *testing.T) *turnBoundsTask {
	t.Helper()
	doc := defaults.Workflow()
	models := []string{"one", "two", "three", "four"}
	for name, role := range doc.Roles {
		role.Routes = nil
		for _, model := range models {
			role.Routes = append(role.Routes, workflow.Binding{Provider: "p", Model: model})
		}
		doc.Roles[name] = role
	}
	j, err := engine.NewLive("bounds", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	x := &turnBoundsTask{t: t, healthy: map[string]bool{}}
	for _, model := range models {
		x.healthy[model] = true
	}
	x.options = engine.Options{SelectRoute: func(routes []workflow.Binding) (workflow.Binding, bool) {
		for _, r := range routes {
			if x.healthy[r.Model] {
				return r, true
			}
		}
		return workflow.Binding{}, false
	}}
	// The attempt limit must not bound these turns unless a test sets it.
	x.j = apply(t, j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: 100})
	return x
}

// dispatch prepares until the task's next dispatch.
func (x *turnBoundsTask) dispatch() *engine.Dispatch {
	x.t.Helper()
	for i := 0; i < 30; i++ {
		j, d, _, err := engine.PrepareWith(x.j, x.options)
		if err != nil {
			x.t.Fatal(err)
		}
		x.j = j
		if d != nil {
			return d
		}
		if j.Parked || j.State == domain.Escalated || j.State.Terminal() {
			x.t.Fatalf("no dispatch: %s parked=%v %q failures %v", j.State, j.Parked, j.Reason, j.Failures)
		}
	}
	x.t.Fatal("no dispatch")
	return nil
}

// complete collects d with one live outcome and round-trips the row, so the
// counters are the durable ones (FAE-ROB-017).
func (x *turnBoundsTask) complete(d *engine.Dispatch, class string) {
	x.t.Helper()
	r := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: class, Detail: "fixture"}
	switch class {
	case "success":
		r.Class, r.Detail, r.Output = "", "", json.RawMessage(`{"accepted":true}`)
	case engine.ClassProviderFailover:
		// The executor records the binding failure, which makes it ineligible.
		x.healthy[d.Sessions[0].Route.Model] = false
		r.Detail = "rate-limited: 429"
	}
	j, _, err := engine.Complete(x.j, d.ID, r)
	if err != nil {
		x.t.Fatal(err)
	}
	x.j = roundTrip(x.t, j)
}

// run dispatches and collects one outcome per class.
func (x *turnBoundsTask) run(classes ...string) {
	x.t.Helper()
	for _, class := range classes {
		x.complete(x.dispatch(), class)
	}
}

func (x *turnBoundsTask) control(c engine.Control) { x.t.Helper(); x.j = apply(x.t, x.j, c) }

func (x *turnBoundsTask) events(kind string, from int) (n int) {
	for _, e := range x.j.History[from:] {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// usedEveryBound is the turn this file ends early: at intake it spends its
// failover rotation, then fails to start twice on the next route (two of
// that route's 3 agent-start attempts; a failover gives the next route
// fresh starts). The durable counters hold both.
func (x *turnBoundsTask) usedEveryBound() {
	x.t.Helper()
	x.run(engine.ClassProviderFailover, "startup-retry", "startup-retry")
	if x.j.State != "intake" || x.j.Parked || agentStarts(x.j, "intake") != 2 || x.j.Failures["failover:intake"] != 1 {
		x.t.Fatalf("fixture turn: %s parked=%v %q failures %v", x.j.State, x.j.Parked, x.j.Reason, x.j.Failures)
	}
}

// freshTurn asserts the next turn, at intake again, has every Appendix A
// per-turn bound: 3 agent-start attempts (two fail and the third launches)
// and 1 failover rotation, after which it completes.
func (x *turnBoundsTask) freshTurn() {
	x.t.Helper()
	from := len(x.j.History)
	x.run("startup-retry", "startup-retry")
	if x.events("park", from) != 0 || x.j.Parked || x.j.State != "intake" || agentStarts(x.j, "intake") != 2 {
		x.t.Fatalf("the next turn did not get 3 agent-start attempts: %s parked=%v %q failures %v", x.j.State, x.j.Parked, x.j.Reason, x.j.Failures)
	}
	x.run(engine.ClassProviderFailover)
	if x.events("failover", from) != 1 || x.events("failure", from) != 0 || x.j.Failures["failover:intake"] != 1 {
		x.t.Fatalf("the next turn did not get its failover rotation: %s %q failures %v", x.j.State, x.j.Reason, x.j.Failures)
	}
	x.run("success")
	if x.j.State == "intake" || x.events("startup-retry", from) != 2 {
		x.t.Fatalf("the next turn did not complete: %s %q", x.j.State, x.j.Reason)
	}
	if n := agentStarts(x.j, "intake"); n != 0 {
		x.t.Fatalf("the turn kept %d agent starts", n)
	}
	for _, key := range []string{"failover:intake", engine.StallRecoveryKey} {
		if x.j.Failures[key] != 0 {
			x.t.Fatalf("completed turn kept %s=%d", key, x.j.Failures[key])
		}
	}
}

// FAE-RT-004, FAE-RT-010, FAE-ROB-017, Appendix A: agent start (3 attempts)
// and failover rotations (1) are bounded per turn. A turn that ends in a
// recorded failure has ended, so the stage's next turn gets both bounds
// afresh, whichever the failed turn spent; its charges are kept (FAE-WF-074).
func TestTurnBoundsRestartAfterAFailedTurn(t *testing.T) {
	spent := map[string][]string{
		"agent-start attempts and rotation": {"startup-retry", "startup-retry", engine.ClassProviderFailover},
		"rotation only":                     {engine.ClassProviderFailover},
		"agent-start attempts only":         {"startup-retry", "startup-retry"},
	}
	for name, classes := range spent {
		t.Run(name, func(t *testing.T) {
			x := newTurnBoundsTask(t)
			x.run(classes...)
			x.run(engine.ClassMalformedOutput)
			failedTurn := x.j
			if failedTurn.State != "intake" || failedTurn.Parked || failedTurn.Attempts != 1 || failedTurn.Counts["intake"] != 1 {
				t.Fatalf("failed turn: %s parked=%v attempts %d visits %d", failedTurn.State, failedTurn.Parked, failedTurn.Attempts, failedTurn.Counts["intake"])
			}
			x.freshTurn()
			// Clearing the per-turn bounds refunded nothing: both turns are charged.
			if x.j.Attempts != 2 || x.j.Counts["intake"] != 2 {
				t.Fatalf("budgets after two intake turns: attempts %d visits %d", x.j.Attempts, x.j.Counts["intake"])
			}
		})
	}
}

// FAE-RT-004, FAE-RT-010, FAE-ROB-017, FAE-WF-074: an escalation ends the
// turn too, however it is reached and left, so the turn after a restart or
// resume gets 3 agent-start attempts and its failover rotation. The control
// carries every per-task budget unrefunded.
func TestTurnBoundsRestartAfterEscalation(t *testing.T) {
	cases := map[string]func(x *turnBoundsTask){
		"StallExhausted then restart": func(x *turnBoundsTask) {
			x.usedEveryBound()
			x.run("stall-retry", "stall-retry", "stall-retry")
			if !x.j.Escalated(domain.StallExhausted) {
				x.t.Fatalf("fixture: %s %q", x.j.State, x.j.Reason)
			}
			x.control(engine.Control{Method: "RestartWithInformation", Information: "try again"})
		},
		"operator escalation then restart": func(x *turnBoundsTask) {
			x.usedEveryBound()
			x.control(engine.Control{Method: "Escalate", Reason: "operator look"})
			x.control(engine.Control{Method: "RestartWithInformation", Information: "carry on"})
		},
		"held startup retry then restart": func(x *turnBoundsTask) {
			x.run("startup-retry", engine.ClassProviderFailover)
			d := x.dispatch()
			x.control(engine.Control{Method: "Escalate", Reason: "operator look"})
			// The held dispatch fails to start; a held turn is never retried.
			x.complete(d, "startup-retry")
			if x.j.State != domain.Escalated || x.j.InFlight != nil {
				x.t.Fatalf("held collection: %s in flight %v", x.j.State, x.j.InFlight != nil)
			}
			x.control(engine.Control{Method: "RestartWithInformation", Information: "carry on"})
		},
		"AttemptCap then resume": func(x *turnBoundsTask) {
			x.control(engine.Control{Method: "SetAttemptLimit", AttemptLimit: 1})
			x.usedEveryBound()
			x.run(engine.ClassMalformedOutput)
			// The retry is a repeated execution the limit refuses.
			if j, d, _, err := engine.PrepareWith(x.j, x.options); err != nil || d != nil || !j.Escalated(domain.AttemptCap) {
				x.t.Fatalf("fixture: %v %s %q", err, j.State, j.Reason)
			} else {
				x.j = roundTrip(x.t, j)
			}
			x.control(engine.Control{Method: "SetAttemptLimit", AttemptLimit: 100})
			x.control(engine.Control{Method: "Resume", Reason: "raised"})
		},
	}
	for name, end := range cases {
		t.Run(name, func(t *testing.T) {
			x := newTurnBoundsTask(t)
			end(x)
			if x.j.State != "intake" || x.j.Parked {
				t.Fatalf("not restarted at intake: %s parked=%v %q", x.j.State, x.j.Parked, x.j.Reason)
			}
			before := x.j
			x.freshTurn()
			// The new turn is charged on top of the carried budgets.
			if x.j.Attempts != before.Attempts+1 || x.j.Counts["intake"] != before.Counts["intake"]+1 || x.j.TestRuns != before.TestRuns {
				t.Fatalf("budgets: attempts %d->%d visits %d->%d", before.Attempts, x.j.Attempts, before.Counts["intake"], x.j.Counts["intake"])
			}
		})
	}
}

// FAE-RT-004, FAE-RT-005, FAE-RT-010, FAE-ROB-017: an operator redispatch of
// an UncertainDispatch park ends the parked turn, so the redispatched turn is
// a new one: 3 agent-start attempts, its failover rotation and 2 stall
// retries, whatever the parked turn spent. It is charged on top of the
// carried budgets, and the crash-recovery count carries.
func TestTurnBoundsRestartAfterUncertainRedispatch(t *testing.T) {
	x := newTurnBoundsTask(t)
	x.usedEveryBound()
	x.run("stall-retry")
	d := x.dispatch()
	x.complete(d, "uncertain")
	if !x.j.Parked || x.j.Reason != "UncertainDispatch" || x.j.Failures[engine.StallRecoveryKey] != 1 || agentStarts(x.j, "intake") != 2 || x.j.Failures["failover:intake"] != 1 {
		t.Fatalf("fixture: parked=%v %q failures %v", x.j.Parked, x.j.Reason, x.j.Failures)
	}
	x.j.Failures["crash-recovery"] = 1
	before := x.j
	j, events, err := engine.ResolveUncertain(x.j, d.ID, "redispatch", nil)
	if err != nil || len(events) != 1 || events[0].Kind != "uncertainty-resolved" {
		t.Fatal(err, events)
	}
	x.j = roundTrip(t, j)
	if n := agentStarts(x.j, "intake"); n != 0 {
		x.t.Fatalf("the turn kept %d agent starts", n)
	}
	for _, key := range []string{"failover:intake", engine.StallRecoveryKey} {
		if x.j.Failures[key] != 0 {
			t.Fatalf("redispatch kept the parked turn's %s=%d", key, x.j.Failures[key])
		}
	}
	if x.j.Failures["crash-recovery"] != 1 || x.j.Attempts != before.Attempts {
		t.Fatalf("redispatch changed a per-task budget: failures %v attempts %d->%d", x.j.Failures, before.Attempts, x.j.Attempts)
	}
	// Two stalls stay within the new turn's 2 retries.
	x.run("stall-retry", "stall-retry")
	if x.j.State != "intake" || x.j.Failures[engine.StallRecoveryKey] != 2 {
		t.Fatalf("the redispatched turn did not get 2 stall retries: %s %q failures %v", x.j.State, x.j.Reason, x.j.Failures)
	}
	x.freshTurn()
	// Each stall retry and the completed launch are charged; nothing the
	// parked turn spent is refunded.
	if x.j.Attempts != before.Attempts+3 || x.j.Counts["intake"] != before.Counts["intake"]+3 {
		t.Fatalf("budgets: attempts %d->%d visits %d->%d", before.Attempts, x.j.Attempts, before.Counts["intake"], x.j.Counts["intake"])
	}
}

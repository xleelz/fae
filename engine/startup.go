package engine

import (
	"errors"
	"github.com/xleelz/fae/workflow"
)

// Startup failures have their own durable bound; they do not spend task attempts
// or stage iteration budgets. Sequence is deliberately retained for fresh IDs.
func completeStartupRetry(input, j Job, d Dispatch, r Result, events []Event) (Job, []Event, error) {
	if j.ExecutionKind != "live" || r.ExecutionKind != "live" || (d.Config.Primitive != workflow.AgentTurn && d.Config.Primitive != workflow.Review) || r.NewTree != "" || r.NewBase != "" || len(r.Output) != 0 || len(r.Reviews) != 0 {
		return input, nil, errors.New("invalid startup recovery")
	}
	key := "startup-recovery:" + d.Stage
	j.Failures[key]++
	if d.AttemptCharged {
		j.Attempts--
	}
	j.Counts[d.Stage]--
	if d.Config.TestRun {
		j.TestRuns--
	}
	j.InFlight = nil
	if j.Failures[key] >= 3 {
		stop(&j, "StartupExhausted", &events)
		return j, events, nil
	}
	event(&j, Event{Kind: "startup-retry", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "positive nonexecution proof and retired transport"}, &events)
	return j, events, nil
}

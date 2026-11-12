package engine

import (
	"errors"
	"fmt"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// ClassRefusedBeforeLaunch is a dispatch refused before anything it
// dispatches started: no provider, host command, identity measurement or VCS
// effect ran. The trusted bridge's refusal before it launched the provider
// (task scope, brief, role policy, a local step), the daemon's refusal of a
// task scope that cannot hold the dispatch, and any failure before the first
// launch (resolving the task's binding or tree, loading feedback, a role with
// no route or brief) are refused before launch; so is a later review session
// the bridge refused before its provider ran. Nothing ran, so it charges no
// attempt, visit or test run (FAE-RT-004, FAE-WF-074, operator ruling
// 2026-10-29).
const ClassRefusedBeforeLaunch = "refused-before-launch"

// refusedBeforeLaunchRepeat is the same-gate reason every refusal before
// launch at a stage shares (`same-gate:<stage>:refused-before-launch`).
const refusedBeforeLaunchRepeat = "refused-before-launch"

// completeRefusedBeforeLaunch refunds the dispatch and records the refusal as
// the stage's failure (a `failure` event whose cause starts
// "RefusedBeforeLaunch: "). Nothing in the task changed, so the next dispatch
// meets the same refusal unless something outside the task does: refusals
// before launch at a stage count toward SameGate as one reason, whatever
// their detail, and the fourth in a row escalates SameGate (Appendix A repeat
// bound 3). A refund leaves the attempt limit nothing to bound, so this count
// is what keeps the retries finite, also when the detail varies. Like any
// same-gate count, a success at the stage or a failure for another reason
// restarts it; such a failure spends an attempt, so the attempt limit still
// bounds the whole. The turn's agent-start, failover and stall bounds carry:
// no turn ran. A review dispatch's earlier sessions that ran are not recorded,
// and the next dispatch runs every reviewer again (FAE-WF-033).
func completeRefusedBeforeLaunch(input, j Job, d Dispatch, r Result, events []Event) (Job, []Event, error) {
	if r.ExecutionKind != "live" || len(r.Output) != 0 || r.NewTree != "" || r.NewBase != "" || len(r.Reviews) != 0 && (d.Config.Primitive != workflow.Review || !reviewsBeforeFailure(d, r.Reviews)) {
		return input, nil, errors.New("invalid refusal before launch")
	}
	refund(&j, d)
	j.InFlight = nil
	reason := "RefusedBeforeLaunch: " + bounded(r.Detail)
	j.Reason = reason
	event(&j, Event{Kind: "failure", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: reason + reviewsRanCause(d, r.Reviews, false)}, &events)
	if n := repeat(&j, d.Stage, refusedBeforeLaunchRepeat); n > SameGateRepeats {
		stop(&j, domain.SameGate, fmt.Sprintf("%s failed %d times for the same reason: refused before launch each time, charging no attempt; the last refusal: %s", d.Stage, n, reason), &events)
	}
	return j, events, nil
}

package engine

import (
	"errors"
	"fmt"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
	"time"
)

// ResolveUncertain never refunds consumed execution budgets. The caller must
// establish operator authority, retired workloads and unchanged physical tree.
func ResolveUncertain(input Job, dispatchID, action string, result *Result) (Job, []Event, error) {
	if input.Fan != nil && input.Fan.Baseline != nil {
		return branchDispatchControl(input, dispatchID, func(view Job) (Job, []Event, error) { return ResolveUncertain(view, dispatchID, action, result) })
	}
	j := clone(input)
	if err := Validate(j); err != nil {
		return input, nil, err
	}
	if !j.Parked || j.Reason != "UncertainDispatch" || j.InFlight == nil || j.InFlight.ID != dispatchID {
		return input, nil, errors.New("not the current uncertain dispatch")
	}
	d := *j.InFlight
	if action != "accept" && result != nil {
		return input, nil, errors.New("result is only valid for accept")
	}
	events := []Event{}
	switch action {
	case "redispatch":
		if j.OperatorStage != "" {
			return input, nil, errors.New("operator-held uncertain dispatch requires adoption or escalation")
		}
		if j.Failures["crash-recovery"] >= 3 {
			return input, nil, errors.New("crash recovery exhausted; escalate required")
		}
		j.InFlight = nil
		j.Parked = false
		j.Reason = ""
		// The parked turn has ended; the redispatch is a new turn, charged
		// a new attempt, with fresh agent-start, failover and stall bounds
		// (clearTurnBounds). Every per-task budget carries, crash recoveries
		// included.
		clearTurnBounds(&j)
	case "escalate":
		j.OperatorStage = ""
		j.OperatorReason = ""
		kind := domain.OperatorEscalate
		if j.Failures["crash-recovery"] >= 3 {
			kind = domain.DispatchRecoveryExhausted
		}
		stop(&j, kind, "UncertainDispatch "+d.ID, &events)
	case "accept":
		if result == nil || result.Class != "" || result.NewTree != "" || result.NewBase != "" || result.ExecutionKind != "live" || result.ProducedAt.After(time.Now().UTC()) || workflow.ProducesChanges(j.Document, d.Config) || (d.Config.Primitive != workflow.AgentTurn && d.Config.Primitive != workflow.Review) {
			return input, nil, errors.New("accept requires a recorded read-only provider completion")
		}
		var err error
		j.Parked = false
		j.Reason = ""
		j, events, err = Complete(j, dispatchID, *result)
		if err != nil {
			return input, nil, err
		}
		if len(j.Provenance) != len(input.Provenance)+len(d.Sessions) {
			return input, nil, errors.New("recorded output did not validate completely")
		}
	default:
		return input, nil, errors.New("invalid uncertainty resolution")
	}
	j.ResolvedDispatches = append(j.ResolvedDispatches, d)
	event(&j, Event{Kind: "uncertainty-resolved", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: action}, &events)
	return j, events, Validate(j)
}

// ResolveUncertainNonExecution redispatches an uncertain dispatch whose
// durable daemon evidence proves the workload never executed (FAE-RT-004):
// its attempt, visit and test-run charges are refunded so the redispatch does
// not double-charge a turn that never ran.
func ResolveUncertainNonExecution(input Job, dispatchID string) (Job, []Event, error) {
	j := clone(input)
	if err := Validate(j); err != nil {
		return input, nil, err
	}
	if !j.Parked || j.Reason != "UncertainDispatch" || j.InFlight == nil || j.InFlight.ID != dispatchID {
		return input, nil, errors.New("not the current uncertain dispatch")
	}
	if j.OperatorStage != "" {
		return input, nil, errors.New("operator-held uncertain dispatch requires adoption or escalation")
	}
	if j.Failures["crash-recovery"] >= 3 {
		return input, nil, errors.New("crash recovery exhausted; escalate required")
	}
	d := *j.InFlight
	events := []Event{}
	refund(&j, d)
	j.InFlight = nil
	j.Parked = false
	j.Reason = ""
	clearTurnBounds(&j)
	j.ResolvedDispatches = append(j.ResolvedDispatches, d)
	event(&j, Event{Kind: "uncertainty-resolved", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "redispatch"}, &events)
	return j, events, Validate(j)
}

// ReplayUnlaunched reissues an in-flight turn whose durable admission journal
// and effect receipts prove no session or effect ever reached its launch
// boundary before an unplanned stop (FAE-RT-004): its attempt, visit and
// test-run charges are refunded and no recovery budget is consumed.
func ReplayUnlaunched(input Job, dispatchID string) (Job, []Event, error) {
	if err := Validate(input); err != nil {
		return input, nil, err
	}
	d := input.InFlight
	if d == nil || d.ID != dispatchID || input.OperatorStage != "" || input.State.Terminal() || input.State == domain.Escalated || (input.Parked && input.Reason != "UncertainDispatch") {
		return input, nil, errors.New("not a replayable unlaunched dispatch")
	}
	j := clone(input)
	events := []Event{}
	refund(&j, *d)
	j.ResolvedDispatches = append(j.ResolvedDispatches, *d)
	j.InFlight = nil
	j.Parked = false
	j.Reason = ""
	event(&j, Event{Kind: "graceful-replay", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "proven non-execution before launch; no budget charged"}, &events)
	return j, events, Validate(j)
}

// ReplayInterrupted reissues a turn a planned graceful stop interrupted
// (FAE-ROB-011): its attempt, visit and test-run charges are refunded and no
// recovery budget is consumed. The caller must prove the stop marker listed the
// dispatch, the workspace is unchanged and every session is retired. Effects
// with external or VCS writes are never replayed here.
func ReplayInterrupted(input Job, dispatchID string) (Job, []Event, error) {
	if err := Validate(input); err != nil {
		return input, nil, err
	}
	d := input.InFlight
	if d == nil || d.ID != dispatchID || input.OperatorStage != "" || input.State.Terminal() || input.State == domain.Escalated || (input.Parked && input.Reason != "UncertainDispatch") {
		return input, nil, errors.New("not a replayable interrupted dispatch")
	}
	switch d.Config.Primitive {
	case workflow.AgentTurn, workflow.Review, workflow.HostCommand, workflow.FreezeArtifact, workflow.CompareFrozen:
	default:
		return input, nil, errors.New("interrupted effect requires receipt adoption or explicit resolution")
	}
	j := clone(input)
	events := []Event{}
	refund(&j, *d)
	j.ResolvedDispatches = append(j.ResolvedDispatches, *d)
	j.InFlight = nil
	j.Parked = false
	j.Reason = ""
	event(&j, Event{Kind: "graceful-replay", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "planned graceful stop; no budget charged"}, &events)
	return j, events, Validate(j)
}

// AdoptInterruptedEffect completes an interrupted local host or VCS effect from
// its completed daemon receipt during recovery (FAE-ROB-012/-016): the outcome
// is recorded, so nothing re-runs and no budget is charged. The caller must
// prove the receipt is the daemon's own, its workloads retired and every
// postcondition exact. External effects and operator holds are never adopted.
func AdoptInterruptedEffect(input Job, dispatchID string, r Result) (Job, []Event, error) {
	if err := Validate(input); err != nil {
		return input, nil, err
	}
	d := input.InFlight
	if d == nil || d.ID != dispatchID || input.OperatorStage != "" || input.State.Terminal() || input.State == domain.Escalated || (input.Parked && input.Reason != "UncertainDispatch") {
		return input, nil, errors.New("not an interrupted effect")
	}
	switch d.Config.Primitive {
	case workflow.HostCommand, workflow.FreezeArtifact, workflow.CompareFrozen, workflow.VcsCommit, workflow.Integrate:
	default:
		return input, nil, errors.New("only a local host or VCS effect adopts its receipt at recovery")
	}
	j := clone(input)
	j.Parked, j.Reason = true, "UncertainDispatch"
	adopted, events, err := adoptEffect(j, dispatchID, r, Event{Kind: "effect-receipt-adopted", Cause: "completed daemon receipt adopted at recovery; no budget charged"})
	if err != nil {
		return input, nil, err
	}
	return adopted, events, nil
}

// RerunInterrupted re-runs, at recovery, a crash-interrupted engine run whose
// re-execution is idempotent: a brokered host command (a test or QA run) or an
// identity measurement (FAE-ROB-013). The interruption is infrastructure, not
// a stage failure, so its attempt, visit and test-run charges are refunded and
// no failure is recorded; the crash consumes the dispatch-recovery budget
// (FAE-ROB-011/-017). The caller must prove no receipt completed, the
// workspace is unchanged and every session workload is retired.
func RerunInterrupted(input Job, dispatchID string) (Job, []Event, error) {
	if err := Validate(input); err != nil {
		return input, nil, err
	}
	d := input.InFlight
	if d == nil || d.ID != dispatchID || input.OperatorStage != "" || input.State.Terminal() || input.State == domain.Escalated || (input.Parked && input.Reason != "UncertainDispatch") {
		return input, nil, errors.New("not a re-runnable interrupted engine run")
	}
	switch d.Config.Primitive {
	case workflow.HostCommand, workflow.FreezeArtifact, workflow.CompareFrozen:
	default:
		return input, nil, errors.New("only an engine run or identity measurement re-runs after a crash")
	}
	j := clone(input)
	events := []Event{}
	refund(&j, *d)
	j.ResolvedDispatches = append(j.ResolvedDispatches, *d)
	j.InFlight = nil
	j.Parked, j.Reason = false, ""
	j.Failures["dispatch-recovery"]++
	event(&j, Event{Kind: "crash-rerun", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "interrupted engine run is infrastructure; re-run on the unchanged tree"}, &events)
	if j.Failures["dispatch-recovery"] > 2 {
		stop(&j, domain.DispatchRecoveryExhausted, fmt.Sprintf("%s crash recoveries exceeded %d", d.Stage, 2), &events)
	}
	return j, events, Validate(j)
}

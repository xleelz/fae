package engine

import (
	"errors"
	"github.com/xleelz/fae/workflow"
	"time"
)

// ResolveUncertain never refunds consumed execution budgets. The caller must
// establish operator authority, retired workloads and unchanged physical tree.
func ResolveUncertain(input Job, dispatchID, action string, result *Result) (Job, []Event, error) {
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
	case "escalate":
		j.OperatorStage = ""
		j.OperatorReason = ""
		stop(&j, "UncertainDispatchEscalated", &events)
	case "accept":
		if result == nil || result.Class != "" || result.NewTree != "" || result.NewBase != "" || result.ExecutionKind != "live" || result.ProducedAt.After(time.Now().UTC()) || d.Config.ChangeProducer || (d.Config.Primitive != workflow.AgentTurn && d.Config.Primitive != workflow.Review) {
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

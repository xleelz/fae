package engine

import (
	"errors"
	"github.com/xleelz/fae/workflow"
)

// AdoptEffect is exclusively for a trusted daemon receipt after revalidating
// physical postconditions and retirement. Operator result JSON must not call it.
func AdoptEffect(input Job, dispatchID string, r Result) (Job, []Event, error) {
	if !input.Parked || input.Reason != "UncertainDispatch" || input.InFlight == nil || input.InFlight.ID != dispatchID {
		return input, nil, errors.New("not current uncertain effect")
	}
	d := *input.InFlight
	switch d.Config.Primitive {
	case workflow.ExternalEffect, workflow.HostCommand, workflow.FreezeArtifact, workflow.CompareFrozen, workflow.VcsCommit, workflow.Integrate:
	default:
		return input, nil, errors.New("unsupported receipt effect")
	}
	if r.ExecutionKind != "live" || r.Class != "" || r.NewBase != "" || r.NewTree != "" {
		return input, nil, errors.New("invalid effect receipt")
	}
	j := clone(input)
	j.Parked = false
	j.Reason = ""
	j, events, e := Complete(j, dispatchID, r)
	if e != nil {
		return input, nil, e
	}
	if len(j.Provenance) != len(input.Provenance)+1 {
		return input, nil, errors.New("recorded effect schema invalid")
	}
	j.ResolvedDispatches = append(j.ResolvedDispatches, d)
	event(&j, Event{Kind: "external-effect-adopted", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "audited trusted receipt with remeasured postconditions"}, &events)
	return j, events, Validate(j)
}

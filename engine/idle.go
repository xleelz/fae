package engine

import (
	"errors"
	"strings"
)

// PaneStarved distinguishes a pane refusal from memory/load/ledger deferrals.
// Only the diagnostic paired with the current park may authorize escalation.
func PaneStarved(j Job) bool {
	if !j.Parked || j.Reason != "infrastructure-deferred" || j.InFlight != nil || j.State.Terminal() {
		return false
	}
	for i := len(j.History) - 1; i >= 0; i-- {
		park := j.History[i]
		if park.Kind != "park" {
			continue
		}
		if park.Cause != "infrastructure-deferred" || park.DispatchID == "" {
			return false
		}
		for k := i - 1; k >= 0; k-- {
			e := j.History[k]
			if e.Kind == "execution-diagnostic" && e.DispatchID == park.DispatchID {
				return strings.Contains(e.Cause, "host admission: PaneLimit")
			}
		}
		return false
	}
	return false
}

// EscalatePaneStarvation is used only after the owned runtime inventory proves
// the limit is saturated by idle sessions and no safe shedding candidate exists.
// Never discard an authoritative in-flight turn or rewrite completed evidence.
func EscalatePaneStarvation(input Job) (Job, []Event, error) {
	if err := Validate(input); err != nil {
		return input, nil, err
	}
	if !PaneStarved(input) {
		return input, nil, errors.New("pane starvation requires an idle infrastructure park")
	}
	j := clone(input)
	events := []Event{}
	stop(&j, "PaneCapStarvation", &events)
	return j, events, Validate(j)
}

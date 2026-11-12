package engine

import (
	"errors"
	"github.com/xleelz/fae/domain"
	"time"
)

// RefuseHostFootprint is a trusted daemon outcome before any resource launch.
func RefuseHostFootprint(input Job, detail string) (Job, []Event, error) {
	if err := Validate(input); err != nil {
		return input, nil, err
	}
	if input.InFlight != nil || input.State.Terminal() {
		return input, nil, errors.New("host footprint refusal requires an idle nonterminal task")
	}
	j := clone(input)
	j.Parked = false
	j.SlotWaitReason = ""
	j.SlotWaitSince = time.Time{}
	events := []Event{}
	stop(&j, domain.FootprintExceedsHost, detail, &events)
	return j, events, nil
}

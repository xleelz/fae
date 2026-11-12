package engine

import (
	"errors"

	"github.com/xleelz/fae/domain"
)

// Fault escalates a valid idle job whose durable stage cannot be decided (for
// example content with no success route), so the drive never re-asks it.
func Fault(input Job, cause error) (Job, []Event, error) {
	if err := Validate(input); err != nil {
		return input, nil, err
	}
	if input.State.Terminal() || input.State == domain.Escalated || input.InFlight != nil {
		return input, nil, errors.New("fault requires an idle non-terminal task")
	}
	j := clone(input)
	events := []Event{}
	stop(&j, domain.HumanDecisionRequired, "EngineFault: "+bounded(cause.Error()), &events)
	return j, events, Validate(j)
}

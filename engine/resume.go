package engine

import (
	"fmt"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// RevalidateResume re-validates, when a stored task resumes, the evidence its
// durable stage stands on (Appendix B ResumeEvidence): every required input
// must still resolve to recorded, provenance-valid evidence of this task. A
// stage whose inputs cannot is escalated ResumeEvidence instead of parking on
// an input nothing will produce. Rows that fail Validate itself cannot carry a
// transition and stay with the caller's quarantine path. A task in flight,
// outside a stage, or already escalated is returned unchanged.
func RevalidateResume(input Job) (Job, []Event, error) {
	if err := Validate(input); err != nil {
		return input, nil, err
	}
	s, ok := input.Document.Stages[string(input.State)]
	if !ok || input.InFlight != nil || input.State.Terminal() || input.State == domain.Escalated {
		return input, nil, nil
	}
	missing := workflow.CheckInputs(s, input.TaskID, latestInputs(input))
	if len(missing) == 0 {
		return input, nil, nil
	}
	j := clone(input)
	events := []Event{}
	stop(&j, domain.ResumeEvidence, fmt.Sprintf("stage %s cannot re-validate inputs %v at resume", j.State, missing), &events)
	return j, events, Validate(j)
}

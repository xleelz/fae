package engine

import (
	"fmt"
	"github.com/xleelz/fae/domain"
)

func PendingDecisions(j Job) []DecisionRequest {
	out := []DecisionRequest{}
	if j.State.Terminal() || j.State == domain.Escalated || j.State == domain.Blocked || j.OperatorStage != "" {
		return out
	}
	if req, ok := pendingDecisionLocal(j); ok {
		out = append(out, req)
	}
	if j.Fan != nil {
		for _, b := range j.Fan.Branches {
			if b.Status != BranchPending && b.Status != BranchRunning {
				continue
			}
			if b.Frame != nil {
				out = append(out, PendingDecisions(*b.Frame)...)
			}
		}
	}
	return out
}
func PendingDecision(j Job) (DecisionRequest, bool) {
	requests := PendingDecisions(j)
	if len(requests) != 1 {
		return DecisionRequest{}, false
	}
	return requests[0], true
}
func Decide(input Job, d Decision) (Job, []Event, DecisionRecord, error) {
	matches := []DecisionRequest{}
	for _, r := range PendingDecisions(input) {
		if (d.ID == "" || r.ID == d.ID) && (d.Branch == "" || r.Branch == d.Branch) {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 {
		return input, nil, DecisionRecord{}, fmt.Errorf("decision refused: select exactly one pending decision by id or branch (matched %d)", len(matches))
	}
	req := matches[0]
	if req.Branch == "" {
		return decideLocal(input, d)
	}
	return decideBranch(input, req, d)
}
func decideBranch(input Job, req DecisionRequest, d Decision) (Job, []Event, DecisionRecord, error) {
	j := clone(input)
	if j.Fan == nil {
		return input, nil, DecisionRecord{}, fmt.Errorf("decision branch absent")
	}
	for i := range j.Fan.Branches {
		b := &j.Fan.Branches[i]
		if b.Frame == nil {
			continue
		}
		view := *b.Frame
		local, ok := pendingDecisionLocal(view)
		var next Job
		var events []Event
		var rec DecisionRecord
		var err error
		if ok && local.ID == req.ID {
			next, events, rec, err = decideLocal(view, d)
		} else if view.Fan != nil {
			found := false
			for _, pending := range PendingDecisions(view) {
				if pending.ID == req.ID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
			next, events, rec, err = decideBranch(view, req, d)
		} else {
			continue
		}
		if err != nil {
			return input, nil, DecisionRecord{}, err
		}
		mergeBranchView(&j, next, events, view)
		b = &j.Fan.Branches[i]
		b.Frame = &next
		b.Stage = string(next.State)
		if string(next.State) == j.Fan.Join {
			finishNestedBranch(&j, i, &next, &events)
		} else if next.State == domain.Escalated || next.State.Terminal() {
			b.Status = BranchFailed
			b.Error = next.Reason
		}
		refreshNestedDispatch(&j)
		if err = settleNestedFan(&j, &events); err != nil {
			return input, nil, DecisionRecord{}, err
		}
		if err = Validate(j); err != nil {
			return input, nil, DecisionRecord{}, err
		}
		return j, events, rec, nil
	}
	return input, nil, DecisionRecord{}, fmt.Errorf("decision refused: branch %s no longer waits", req.Branch)
}

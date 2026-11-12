package engine

import (
	"crypto/sha256"
	"fmt"
)

// CheckpointStorageStage keeps the plane's legacy compound checkpoint key
// compatible while preventing two branch instances from sharing its stage key.
func CheckpointStorageStage(d Dispatch) string {
	if d.BranchInstance == "" {
		return d.Stage
	}
	sum := sha256.Sum256([]byte(d.BranchInstance))
	return fmt.Sprintf("branch:%x:%s", sum[:12], d.Stage)
}
func (j *Job) SaveDispatchCheckpoint(d Dispatch, cp TurnCheckpoint) bool {
	if j == nil {
		return false
	}
	if j.Fan != nil && j.Fan.Baseline != nil {
		for i := range j.Fan.Branches {
			b := &j.Fan.Branches[i]
			if b.Frame != nil && b.Frame.SaveDispatchCheckpoint(d, cp) {
				return true
			}
		}
		return false
	}
	if j.InFlight == nil || j.InFlight.ID != d.ID {
		return false
	}
	cp.Visit, cp.Attempt = d.Visit, d.Attempt
	j.SaveCheckpoint(d.Stage, cp)
	return true
}

// RecoverBranchFrames applies recovery to the authoritative leaf views rather
// than refunding the root's aggregate counters or another branch's alias.
func RecoverBranchFrames(input Job, recover func(Job) (Job, error)) (Job, error) {
	j := clone(input)
	if j.Fan == nil || j.Fan.Baseline == nil {
		return recover(j)
	}
	for i := range j.Fan.Branches {
		b := &j.Fan.Branches[i]
		if b.Frame == nil || b.Status != BranchRunning {
			continue
		}
		before := *b.Frame
		next, err := RecoverBranchFrames(before, recover)
		if err != nil {
			return input, err
		}
		events := append([]Event(nil), next.History[len(before.History):]...)
		mergeBranchView(&j, next, events, before)
		b = &j.Fan.Branches[i]
		b.Frame = &next
		b.InFlight = nil
		if ds := BranchDispatches(next); len(ds) > 0 {
			b.InFlight = ds[0]
		}
	}
	refreshNestedDispatch(&j)
	return j, Validate(j)
}

func branchDispatchControl(input Job, id string, control func(Job) (Job, []Event, error)) (Job, []Event, error) {
	j := clone(input)
	var events []Event
	for i := range j.Fan.Branches {
		b := &j.Fan.Branches[i]
		if b.Frame == nil {
			continue
		}
		view := *b.Frame
		found := false
		for _, d := range BranchDispatches(view) {
			if d.ID == id {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		before := len(view.History)
		var next Job
		var ev []Event
		var err error
		if view.Fan != nil && view.Fan.Baseline != nil {
			next, ev, err = branchDispatchControl(view, id, control)
		} else {
			next, ev, err = control(view)
		}
		if err != nil {
			return input, nil, err
		}
		if ev == nil {
			ev = append([]Event(nil), next.History[before:]...)
		}
		mergeBranchView(&j, next, ev, view)
		events = append(events, ev...)
		b = &j.Fan.Branches[i]
		b.Frame = &next
		b.InFlight = nil
		if ds := BranchDispatches(next); len(ds) > 0 {
			b.InFlight = ds[0]
		}
		refreshNestedDispatch(&j)
		return j, events, Validate(j)
	}
	return input, nil, fmt.Errorf("stale branch dispatch %s", id)
}

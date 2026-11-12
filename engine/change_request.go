package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/xleelz/fae/workflow"
)

type AncestorChangeRequest struct {
	ID          string    `json:"id"`
	Source      string    `json:"source"`
	Target      string    `json:"target"`
	Artifact    string    `json:"artifact"`
	Value       any       `json:"value"`
	State       string    `json:"state"`
	ForwardedBy string    `json:"forwarded_by,omitempty"`
	Evidence    []string  `json:"evidence"`
	Reason      string    `json:"reason,omitempty"`
	Message     string    `json:"authorizing_message,omitempty"`
	Actor       string    `json:"actor,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func changeRequestStep(j *Job, s workflow.Stage, events *[]Event) error {
	stage := string(j.State)
	target := s.Target
	if target == "parent" && j.Parent != nil {
		target = j.Parent.Task
	}
	if target == "" || !slices.Contains(j.Ancestors, target) {
		return routeFailure(j, "change_request", "target is not an ancestor", "", events)
	}
	if s.Value == nil {
		return routeFailure(j, "binding", "change request value is missing", "", events)
	}
	value, ids, err := ResolveProjection(*j, *s.Value)
	if err != nil {
		return routeFailure(j, "binding", err.Error(), "", events)
	}
	id := fmt.Sprintf("%s.%s.%d", j.TaskID, stage, j.Counts[stage])
	if j.BranchContext != nil {
		id += "." + j.BranchContext.Path + fanVisitSuffix(j.BranchContext)
	} else if j.Fan != nil {
		if _, b := j.Fan.activeBranch(); b != nil {
			id += "." + b.ID
		}
	}
	req := AncestorChangeRequest{ID: id, Source: j.TaskID, Target: target, Artifact: s.Artifact, Value: value, State: "pending", Evidence: ids, CreatedAt: time.Now().UTC()}
	if projectionForwardsRequest(*s.Value) {
		req.ForwardedBy = j.TaskID
	}
	for _, prior := range j.AncestorRequests {
		if prior.ID == id {
			a, _ := json.Marshal(prior.Value)
			b, _ := json.Marshal(value)
			if prior.Target != target || string(a) != string(b) {
				return routeFailure(j, "change_request", "request identity already records a different immutable value", "", events)
			}
			next, err := route(*j, s)
			if err != nil {
				return err
			}
			return move(j, next, "existing change request", events)
		}
	}
	j.AncestorRequests = append(j.AncestorRequests, req)
	event(j, Event{Kind: "change-request", Stage: stage, Attempt: j.stagedAttempt(), Cause: fmt.Sprintf("request %s addressed to %s", id, target), Request: &req}, events)
	next, err := route(*j, s)
	if err != nil {
		return err
	}
	return move(j, next, "change request recorded", events)
}

func projectionForwardsRequest(p workflow.Projection) bool {
	if p.Source == "change_requests" {
		return true
	}
	for _, child := range p.Object {
		if projectionForwardsRequest(child) {
			return true
		}
	}
	return false
}

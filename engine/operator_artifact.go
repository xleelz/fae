package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
	"strings"
	"time"
)

// SupplyArtifact consumes one explicitly supplyable AgentTurn output without
// launching a role session. Kernel evidence and downstream gates remain normal.
func SupplyArtifact(input Job, stage string, raw json.RawMessage, message, actor string) (Job, []Event, error) {
	j := clone(input)
	events := []Event{}
	if err := Validate(j); err != nil {
		return input, nil, err
	}
	if j.InFlight != nil || j.State.Terminal() || j.State == domain.Escalated || j.State == domain.Blocked {
		return input, nil, errors.New("artifact supply requires a pending or parked AgentTurn with no dispatch")
	}
	target := string(j.State)
	if j.State == domain.Pending {
		target = j.Document.Start
	}
	if stage != "" && stage != target {
		return input, nil, fmt.Errorf("artifact supply stage %s is not pending stage %s", stage, target)
	}
	s := j.Document.Stages[target]
	if s.Primitive != workflow.AgentTurn || !s.OperatorMaySupply {
		return input, nil, errors.New("stage does not permit operator-supplied artifacts")
	}
	if workflow.ProducesChanges(j.Document, s) {
		return input, nil, errors.New("operator-supplied artifact cannot replace a writable stage")
	}
	if strings.TrimSpace(message) == "" {
		return input, nil, errors.New("artifact supply requires an authorizing message")
	}
	if missing := workflow.CheckInputs(s, j.TaskID, latestInputs(j)); len(missing) > 0 {
		return input, nil, fmt.Errorf("artifact supply inputs: %v", missing)
	}
	typ := j.Document.Artifacts[s.Output]
	if err := workflow.ValidateArtifact(typ.Schema, raw); err != nil {
		return input, nil, fmt.Errorf("artifact schema %s: %w", s.Output, err)
	}
	var fields map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return input, nil, errors.New("operator artifact must be an object")
	}
	canonical, _ := json.Marshal(fields)
	id, err := domain.ContentHash(canonical)
	if err != nil {
		return input, nil, err
	}
	if actor == "" {
		actor = "operator"
	}
	if j.State == domain.Pending {
		if err := move(&j, target, "operator artifact entry", &events); err != nil {
			return input, nil, err
		}
	}
	j.Counts[target]++
	session := "operator:" + message
	dispatch := fmt.Sprintf("%s/%s/%d", session, target, j.Counts[target])
	p := Provenance{ExecutionKind: j.ExecutionKind, ArtifactID: id, DispatchID: dispatch, TaskID: j.TaskID, Stage: target, Primitive: workflow.AgentTurn, Role: "operator", Session: session, Tree: j.Tree, ProducedAt: time.Now().UTC(), WorkflowHash: j.WorkflowHash, WorkflowRevision: j.Revision}
	for _, a := range boundInputs(j, s) {
		p.Inputs = append(p.Inputs, a.ID)
	}
	j.Evidence = append(j.Evidence, workflow.Evidence{ID: id, TaskID: j.TaskID, Type: s.Output, Version: typ.Version, Stage: target, Tree: j.Tree, Role: "operator", Session: session, Provenance: true, Fields: fields})
	j.Provenance = append(j.Provenance, p)
	j.Parked, j.Reason = false, ""
	event(&j, Event{Kind: "operator-artifact", Stage: target, Attempt: j.stagedAttempt(), Cause: "supplied by " + actor + " citing " + message, DispatchID: dispatch}, &events)
	next, err := route(j, s)
	if err != nil {
		return input, nil, err
	}
	if err = move(&j, next, "completed "+dispatch, &events); err != nil {
		return input, nil, err
	}
	return j, events, Validate(j)
}

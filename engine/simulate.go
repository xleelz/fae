package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// Simulate is the FAE-WF-091 dry-run simulator: the durable engine itself,
// driven offline on a scripted adapter that replays the declared role outputs
// and host-command results, so a simulated path is the path the engine takes
// (same-gate, progress discounts, request caps and every floor included). It
// performs no I/O: nothing touches a control plane, worktree or provider.
//
// A Local simulation pins the Local projection; a Normal one admits the
// Normal-only stages, whose results are scripted, never performed. A step's
// next may select only a Merge's reverify route (a changed integration base)
// or an Integrate's (a moved target), and a SubWorkflow's failure route; a
// HumanDecision step names the operator's option. A missing script result is
// an error; a stage awaiting a decision or an input ends the simulation there.
func Simulate(d workflow.Document, task string, script workflow.Script, o workflow.SimulationOptions) (workflow.Simulation, error) {
	result := workflow.Simulation{State: domain.Pending}
	if footprint, err := workflow.DeriveFootprint(d, nil, 0); err == nil {
		result.Footprint = &footprint
	}
	if issues := workflow.Validate(d); len(issues) > 0 {
		return result, &workflow.ValidationError{Issues: issues}
	}
	if !o.Normal {
		var err error
		if d, err = workflow.LocalProjection(d); err != nil {
			return result, err
		}
	}
	if err := checkScript(d, task, script); err != nil {
		return result, err
	}
	j, err := NewLive(task, 1, d, "simulated-tree")
	if err != nil {
		return result, err
	}
	j.Disposition = domain.Disposition{Mode: domain.Local}
	if o.Normal {
		j.Disposition = domain.Disposition{Mode: domain.Normal, Live: true, Destination: "simulated", PushNamespace: "simulated/"}
	}
	for i := 0; ; i++ {
		if i > 100000 {
			return result, errors.New("simulation did not terminate")
		}
		if j.State.Terminal() || j.State == domain.Escalated {
			break
		}
		if j.Parked && j.Reason == ParkLoopBackoff {
			j.AdmissionRetryAfter = time.Time{} // timings are abstracted away
		} else if j.Parked {
			var done bool
			if j, done, err = simulatedPark(j, script); err != nil {
				return result, err
			}
			if done {
				break
			}
			continue
		}
		next, dispatch, _, err := PrepareWith(j, Options{})
		if err != nil {
			return result, err
		}
		if j = next; dispatch == nil {
			continue
		}
		r, err := simulatedResult(*dispatch, j.Document.Artifacts[dispatch.Config.Output].Version, script)
		if err != nil {
			return result, err
		}
		if j, _, err = Complete(j, dispatch.ID, r); err != nil {
			return result, err
		}
	}
	result.State, result.Reason, result.Evidence, result.Records = j.State, j.Reason, j.Evidence, j.GateRecords
	for _, e := range j.History {
		switch {
		case e.Kind == "transition" || e.Kind == "branch-transition":
			result.Transitions = append(result.Transitions, workflow.Transition{From: e.From, To: e.To})
			result.Path = append(result.Path, string(e.To))
		case e.Kind == "gate" && e.Gate != nil:
			result.Gates = append(result.Gates, *e.Gate)
		}
	}
	result.Tree = scenarioReport(map[string]Job{j.TaskID: j}, j.TaskID, nil).Tree
	return result, nil
}

// checkScript refuses a script that scripts evaluated stages, mixes decisions
// into other steps, or carries evidence of another task or stage.
func checkScript(d workflow.Document, task string, script workflow.Script) error {
	for id, steps := range script {
		s, ok := d.Stages[id]
		if !ok || !dispatching(s.Primitive) && s.Primitive != workflow.HumanDecision && s.Primitive != workflow.SubWorkflow {
			return fmt.Errorf("script result for non-dispatching stage %s; gates, branches and loops are evaluated", id)
		}
		for _, step := range steps {
			if (s.Primitive == workflow.HumanDecision) != (step.Decision != "" || s.ItemsFrom != nil && step.Items != nil) || s.Primitive == workflow.HumanDecision && (step.Next != "" || len(step.Evidence) > 0) {
				return fmt.Errorf("script for %s: a HumanDecision step names exactly one decision and nothing else", id)
			}
			for _, a := range step.Evidence {
				if a.ID == "" || a.TaskID != task || a.Stage != id || !a.Provenance {
					return fmt.Errorf("invalid scripted evidence provenance for %s", id)
				}
			}
		}
	}
	return nil
}
func dispatching(p workflow.Primitive) bool {
	switch p {
	case workflow.AgentTurn, workflow.Review, workflow.HostCommand, workflow.FreezeArtifact, workflow.CompareFrozen, workflow.VcsCommit, workflow.Merge, workflow.Integrate, workflow.ExternalEffect:
		return true
	}
	return false
}

// simulatedPark resolves a park from the script: an operator decision or a
// sub-workflow's mapped result. A decision or input nobody scripted ends the
// simulation.
func simulatedPark(j Job, script workflow.Script) (Job, bool, error) {
	if req, ok := PendingDecision(j); ok {
		if req.Visit > len(script[req.Stage]) {
			return j, true, nil
		}
		next, _, _, err := Decide(j, Decision{Option: script[req.Stage][req.Visit-1].Decision, Items: script[req.Stage][req.Visit-1].Items, Message: "simulated", Actor: "operator"})
		return next, false, err
	}
	if ref, ok := PendingChild(j); ok {
		if ref.Visit > len(script[ref.Stage]) {
			return j, false, fmt.Errorf("missing script result for sub-workflow %s visit %d", ref.Stage, ref.Visit)
		}
		step, s := script[ref.Stage][ref.Visit-1], j.Document.Stages[ref.Stage]
		if step.Next != "" {
			if step.Next != s.Failure || s.Failure == "" || len(step.Evidence) > 0 {
				return j, false, fmt.Errorf("script for %s may only select its failure route, without evidence", ref.Stage)
			}
			next, _, err := ChildFailed(j, "simulated child failure")
			return next, false, err
		}
		typ := j.Document.Artifacts[s.Output]
		if len(step.Evidence) != 1 || step.Evidence[0].Type != s.Output || step.Evidence[0].Version != typ.Version {
			return j, false, fmt.Errorf("script for %s needs one %s artifact of this task and stage", ref.Stage, s.Output)
		}
		raw, _ := json.Marshal(step.Evidence[0].Fields)
		if err := workflow.ValidateArtifact(typ.Schema, raw); err != nil {
			return j, false, fmt.Errorf("sub-workflow artifact for %s: %w", ref.Stage, err)
		}
		next, events := clone(j), []Event{}
		if err := bindChildOutput(&next, ref, raw, nil, "simulated child "+ref.Task, &events); err != nil {
			return j, false, err
		}
		return next, false, Validate(next)
	}
	if j.Reason == "" {
		return j, false, errors.New("parked without a reason")
	}
	// MissingInput and every other wait leave the simulation at the stage.
	return j, true, nil
}

// simulatedResult is the scripted adapter: the step of this stage visit as
// the executor result. A step whose records do not fit the stage (count,
// type or version) yields a malformed output the engine retries and bounds.
func simulatedResult(d Dispatch, version int, script workflow.Script) (Result, error) {
	r := Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC()}
	steps := script[d.Stage]
	if d.Visit > len(steps) {
		if d.Config.Primitive != workflow.FreezeArtifact && d.Config.Primitive != workflow.CompareFrozen {
			return r, fmt.Errorf("missing script result for %s visit %d", d.Stage, d.Visit)
		}
		// Like the scripted adapter, identities are computed from the bound
		// subject's declared test source rather than scripted.
		var err error
		if r.Output, err = rehearsedIdentity(d); err != nil {
			r.Class, r.Detail = "execution-error", err.Error()
		}
		return r, nil
	}
	step := steps[d.Visit-1]
	if step.Next != "" {
		if step.Next != d.Config.Reverify || d.Config.Reverify == "" {
			return r, fmt.Errorf("script may only select the reverify route of %s", d.Stage)
		}
		if d.Config.Primitive == workflow.Integrate {
			r.Class = "target-moved"
			return r, nil
		}
		r.NewBase = fmt.Sprintf("simulated-base-%d", d.Visit)
	}
	outputs := []json.RawMessage{}
	for _, a := range step.Evidence {
		raw, _ := json.Marshal(a.Fields)
		if a.Type != d.Config.Output || a.Version != version {
			raw = json.RawMessage(`"scripted artifact type or version differs from the stage output"`)
		}
		outputs = append(outputs, raw)
	}
	if d.Config.Primitive != workflow.Review {
		r.Output = json.RawMessage(`"one scripted record required"`)
		if len(outputs) == 1 {
			r.Output = outputs[0]
		}
		return r, nil
	}
	if len(outputs) == len(d.Sessions) {
		for i, s := range d.Sessions {
			r.Reviews = append(r.Reviews, ReviewResult{Session: s.ID, Role: s.Role, Output: outputs[i]})
		}
	}
	return r, nil
}

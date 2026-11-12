package engine

import (
	"errors"
	"fmt"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// Migrate moves a quiescent task to another revision of its workflow
// (FAE-WF-093). The mapped position is the stage the task will next run: its
// current stage, a Blocked task's remediation route, or the start stage of a
// Pending or Escalated task (Escalated leaves only for the first stage,
// FAE-WF-021). A named stage that differs from the current one must be a legal
// edge of the new revision's graph (FAE-WF-008). It refuses when the stored
// evidence does not fit the new revision or the mapped stage lacks a required
// input. In-flight and terminal tasks never move.
func Migrate(input Job, doc workflow.Document, revision int, stage string) (Job, []Event, error) {
	j := clone(input)
	events := []Event{}
	fail := func(format string, args ...any) (Job, []Event, error) {
		return input, nil, fmt.Errorf("migration refused: "+format, args...)
	}
	if err := Validate(j); err != nil {
		return input, nil, err
	}
	if !CanChangeTask(j.State.Terminal(), j.InFlight != nil, false) {
		return fail("task is terminal or has a dispatch in flight; it finishes on its pinned revision")
	}
	if doc.ID != j.Document.ID {
		return fail("revision belongs to workflow %q, not the task's workflow %q", doc.ID, j.Document.ID)
	}
	if revision < 1 || revision == j.Revision {
		return fail("requires a different revision than %d", j.Revision)
	}
	if issues := workflow.Validate(doc); len(issues) > 0 {
		return fail("invalid revision %d: %v", revision, issues)
	}
	if len(j.ReviewStages) > 0 || j.OperatorStage != "" {
		return fail("review-lifecycle and held tasks keep their pinned revision")
	}
	if j.Fan != nil {
		return fail("a task inside a fan-out keeps its pinned revision until the join")
	}
	if _, waiting := PendingChild(j); waiting {
		return fail("a task waiting for its sub-workflow keeps its pinned revision")
	}
	hash, err := workflow.Hash(doc)
	if err != nil {
		return input, nil, err
	}
	exists := func(s string) bool { _, ok := doc.Stages[s]; return ok }
	position, at := "", j.State
	switch j.State {
	case domain.Pending, domain.Escalated:
		if stage != "" && stage != doc.Start {
			return fail("a %s task maps only to the revision's start stage", j.State)
		}
		position = doc.Start
		j.CarryEntry = ""
	case domain.Blocked:
		if stage != "" {
			j.BlockedRoute = stage
		}
		if !exists(j.BlockedFrom) {
			return fail("blocked stage %s is absent from revision %d", j.BlockedFrom, revision)
		}
		position = j.BlockedRoute
		if exists(position) && !workflow.CanTransition(doc, domain.Blocked, domain.State(position)) {
			return fail("stage %s is not a remediation route of revision %d", position, revision)
		}
	default:
		if stage == "" {
			stage = string(j.State)
		}
		if !exists(string(j.State)) {
			return fail("stage %s is absent from revision %d; block or escalate the task before migrating it", j.State, revision)
		}
		position, at = stage, domain.State(stage)
	}
	if !exists(position) {
		return fail("mapped stage %q is absent from revision %d; name the mapped stage", position, revision)
	}
	j.Document, j.Revision, j.WorkflowHash = doc, revision, hash
	if at != j.State {
		// A remap within the new revision is an edge of its graph.
		if err := enter(&j, at); err != nil {
			return fail("%v in revision %d", err, revision)
		}
	}
	if j.Parked && j.Reason == ParkHumanDecision {
		// A pending decision belongs to the old revision; the mapped stage runs afresh.
		j.Parked, j.Reason = false, ""
	}
	if err := Validate(j); err != nil {
		return input, nil, errors.Join(fmt.Errorf("migration refused: current evidence does not fit revision %d", revision), err)
	}
	if missing := workflow.CheckInputs(doc.Stages[position], j.TaskID, latestInputs(j)); len(missing) > 0 {
		return fail("stage %s of revision %d is missing required inputs %v", position, revision, missing)
	}
	event(&j, Event{Kind: "migrated", Stage: position, From: input.State, To: j.State, Attempt: max(1, j.Attempts), Cause: fmt.Sprintf("revision %d -> %d at %s", input.Revision, revision, position)}, &events)
	return j, events, nil
}

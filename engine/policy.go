package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// dispatchArgv binds a templated HostCommand from its dispatch inputs; an
// untemplated stage dispatches its literal vector and records no Argv.
func dispatchArgv(s workflow.Stage, inputs []workflow.Evidence) ([]string, error) {
	if s.Primitive != workflow.HostCommand || len(s.Bindings) == 0 {
		return nil, nil
	}
	return workflow.BindCommand(s, inputs)
}

// checkArgv re-derives a persisted dispatch's bound vector from its inputs.
func checkArgv(d Dispatch) error {
	want, e := dispatchArgv(d.Config, d.Inputs)
	if workflow.HasRuntimeReferences(d.Config.Command) {
		want, e = workflow.BindCommandValues(d.Config, d.Inputs, d.TemplateValues)
	}
	if e != nil || !slices.Equal(want, d.Argv) {
		return errors.New("dispatch command vector differs from its bound inputs")
	}
	return nil
}

// Vector is the exact argument vector a HostCommand dispatch executes.
func (d Dispatch) Vector() []string {
	if len(d.Argv) > 0 {
		return d.Argv
	}
	return d.Config.Command
}

// Typed no-write commit refusals of the target's commit floor.
const (
	ClassNoRedViolation      = "no-red-violation"
	ClassRequiredCheckFailed = "required-check-failed"
)

// commitRefused escalates a typed commit-floor refusal; nothing was written.
// The commit floor is checked before any commit object exists, so the
// dispatch ran nothing: its attempt and visit are refunded (FAE-RT-004,
// operator ruling 2026-10-29), and the escalation stands for the operator.
func commitRefused(input, j Job, d Dispatch, r Result, events []Event) (Job, []Event, bool, error) {
	if r.Class != ClassNoRedViolation && r.Class != ClassRequiredCheckFailed {
		return j, events, false, nil
	}
	if j.ExecutionKind != "live" || r.ExecutionKind != "live" || d.Config.Primitive != workflow.VcsCommit || len(r.Output) != 0 || len(r.Reviews) != 0 || r.NewTree != "" || r.NewBase != "" {
		return input, nil, true, errors.New("invalid commit refusal")
	}
	kind := domain.NoRedViolation
	if r.Class == ClassRequiredCheckFailed {
		kind = domain.HumanDecisionRequired
		r.Detail = "RequiredCheckFailed: " + r.Detail
	}
	refund(&j, d)
	stop(&j, kind, bounded(r.Detail), &events)
	return j, events, true, nil
}

// ClassExternalRefused is a pinned external call the remote side refused.
const ClassExternalRefused = "external-refused"

// effectRefused escalates ExternalEffectRefused for a pinned external call
// the remote side refused (Appendix B); the detail carries any reported id,
// which the daemon also records before typing the refusal.
func effectRefused(input, j Job, d Dispatch, r Result, events []Event) (Job, []Event, bool, error) {
	if r.Class != ClassExternalRefused {
		return j, events, false, nil
	}
	if j.ExecutionKind != "live" || r.ExecutionKind != "live" || d.Config.Primitive != workflow.ExternalEffect || len(r.Output) != 0 || len(r.Reviews) != 0 || r.NewTree != "" || r.NewBase != "" {
		return input, nil, true, errors.New("invalid external refusal")
	}
	stop(&j, domain.ExternalEffectRefused, bounded(r.Detail), &events)
	return j, events, true, nil
}

// recordFollowUps records each output element the stage declares a follow-up
// (FAE-WF-084) as a durable follow-up event naming its artifact; deferred
// findings are then tracked beyond the verdict that deferred them.
func recordFollowUps(j *Job, d Dispatch, records []workflow.Evidence, events *[]Event) {
	f := d.Config.FollowUps
	if f == nil {
		return
	}
	for _, a := range records {
		elements, err := workflow.Elements(a, f.Path, f.When)
		if err != nil {
			event(j, Event{Kind: "follow-up", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: bounded(a.ID + ": unreadable follow-ups: " + err.Error())}, events)
			continue
		}
		for _, element := range elements {
			raw, _ := json.Marshal(element)
			event(j, Event{Kind: "follow-up", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: bounded(a.ID + ": " + string(raw))}, events)
		}
	}
}

// HasFrozenRed reports recorded FreezeArtifact evidence for the task.
func HasFrozenRed(j Job) bool {
	for _, a := range j.Evidence {
		if j.Document.Stages[a.Stage].Primitive == workflow.FreezeArtifact {
			return true
		}
	}
	return false
}

// NoRedDeclared reports a no-red declaration in the latest artifact of a
// stage the workflow permits to declare it (FAE-WF-073).
func NoRedDeclared(j Job) bool {
	for _, a := range latestInputs(j) {
		if slices.Contains(j.Document.NoRedStages, a.Stage) && a.Fields["no_red"] == true {
			return true
		}
	}
	return false
}

// branchEvidence is what a Branch arm reads: current-tree evidence plus the
// task's latest no-red declaration (FAE-WF-073). A declaration is a pre-code
// fact of the task, not of a tree; the validator admits only declaring
// stages no change producer can reach, so no later tree supersedes it.
func branchEvidence(j Job) []workflow.Evidence {
	out := current(j)
	have := map[string]bool{}
	for _, a := range out {
		have[a.Stage] = true
	}
	for _, a := range latestInputs(j) {
		if slices.Contains(j.Document.NoRedStages, a.Stage) && !have[a.Stage] {
			out = append(out, a)
		}
	}
	return out
}

// roleRoutes are the role's routes under the task class overlay.
func (j Job) roleRoutes(name string) []workflow.Binding {
	r, _ := workflow.EffectiveRole(j.Document, j.TaskClass, name)
	return r.Routes
}

// SetTaskClass pins the admitted class, or the workflow's default class.
func SetTaskClass(j Job, class string) (Job, error) {
	if class == "" {
		class = j.Document.DefaultClass
	}
	if _, ok := j.Document.TaskClasses[class]; class != "" && !ok {
		return j, errors.New("unknown task class " + class)
	}
	j.TaskClass = class
	return j, nil
}

// RedPhase reports that the latest test-authoring turn has not yet been
// frozen: a declared-shape run now establishes a (replacement) RED.
func RedPhase(j Job) bool {
	authored, frozen := 0, 0
	for i, e := range j.History {
		if e.Kind != "dispatch" {
			continue
		}
		if s := j.Document.Stages[e.Stage]; s.Phase == "test-first" {
			authored = i + 1
		} else if s.Primitive == workflow.FreezeArtifact {
			frozen = i + 1
		}
	}
	return authored > frozen
}

// LoopProgress is a progress-aware Loop's last measure and the number of
// consecutive entries without a strict reduction.
type LoopProgress struct {
	Last   float64 `json:"last"`
	Streak int     `json:"streak"`
}

// loopProgress applies a Loop's progress declaration (FAE-WF-075): an entry
// that strictly reduces the measured open findings is not charged an
// iteration; a no-improvement streak past its bound escalates LoopCap.
func loopProgress(j *Job, s workflow.Stage, events *[]Event) (stopped, discounted bool) {
	if s.Progress == nil {
		return false, false
	}
	loop := string(j.State)
	m, ok := workflow.Measure(s.Progress.Measure, current(*j))
	if !ok {
		return false, false
	}
	if j.LoopProgress == nil {
		j.LoopProgress = map[string]LoopProgress{}
	}
	prev, seen := j.LoopProgress[loop]
	next := LoopProgress{Last: m}
	if seen && m < prev.Last {
		discounted = true
		event(j, Event{Kind: "loop-progress", Stage: loop, Attempt: j.stagedAttempt(), Cause: fmt.Sprintf("open findings %v -> %v; iteration not charged", prev.Last, m)}, events)
	} else if seen {
		next.Streak = prev.Streak + 1
		if next.Streak > s.Progress.Streak {
			stop(j, domain.LoopCap, fmt.Sprintf("%s made no progress for %d iterations, over its bound of %d; the streak carries for the life of the task, across resume and restart, until an entry makes progress", loop, next.Streak, s.Progress.Streak), events)
			return true, false
		}
	}
	j.LoopProgress[loop] = next
	return false, discounted
}

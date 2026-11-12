package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/xleelz/fae/workflow"
)

// ParkLoopBackoff is the park reason of a Loop waiting out its declared
// backoff before a repeated iteration.
const ParkLoopBackoff = "LoopBackoff"

// record evaluates x into a content-addressed gate record, appends it and its
// event, and returns the result the caller routes on (FAE-WF-063).
func record(j *Job, kind, stage, name string, arm int, x workflow.Expr, evidence []workflow.Evidence, eventKind string, events *[]Event) workflow.GateResult {
	if expressionReadsTree(x) {
		recordTaskDataEvidence(j, evidence)
	}
	r := workflow.Evaluate(kind, stage, name, arm, j.Tree, x, evidence)
	j.GateRecords = append(j.GateRecords, r)
	g := r.Result
	cause := "evaluated"
	if arm > 0 {
		cause = fmt.Sprintf("evaluated arm %d", arm)
	}
	event(j, Event{Kind: eventKind, Stage: stage, Attempt: j.stagedAttempt(), Cause: cause, Gate: &g, Record: r.ID}, events)
	return g
}

// failedCheck records the aggregate verdict of an aggregated Review or the
// expected-shape check of a shaped HostCommand over the batch just recorded,
// and reports whether the stage must take its failure route.
func failedCheck(j *Job, d Dispatch, s workflow.Stage, events *[]Event) bool {
	var g workflow.GateResult
	switch {
	case s.Primitive == workflow.Review && s.Aggregation != "":
		g = record(j, workflow.RecordAggregate, d.Stage, "", 0, workflow.AggregateExpr(d.Stage, s), current(*j), "review-aggregate", events)
	case s.Primitive == workflow.HostCommand && s.ExpectedShape != nil:
		g = record(j, workflow.RecordShape, d.Stage, "", 0, workflow.ShapeExpr(d.Stage, *s.ExpectedShape), current(*j), "expected-shape", events)
	default:
		return false
	}
	return !g.Passed
}

// backoff parks a Loop before a repeated iteration for its declared delay;
// Prepare resumes the iteration once the delay has elapsed.
func backoff(j Job, s workflow.Stage, events []Event) (Job, *Dispatch, []Event, error) {
	j.Parked = true
	j.Reason = ParkLoopBackoff
	j.AdmissionRetryAfter = time.Now().UTC().Add(time.Duration(s.BackoffSeconds) * time.Second)
	event(&j, Event{Kind: "park", Stage: string(j.State), Attempt: j.stagedAttempt(), Cause: fmt.Sprintf("%s %ds before iteration %d", ParkLoopBackoff, s.BackoffSeconds, j.Counts[string(j.State)]+1)}, &events)
	return j, nil, events, nil
}

// validateRecords checks every stored gate record's content address.
func validateRecords(j Job) error {
	for _, r := range j.GateRecords {
		if id, err := r.Digest(); err != nil || id != r.ID {
			return errors.New("gate record content address mismatch")
		}
	}
	return nil
}

// ReplayGates re-evaluates every stored gate record over the job's recorded
// evidence and requires identical results (FAE-WF-063).
func ReplayGates(j Job) error {
	evidence := append([]workflow.Evidence(nil), j.Evidence...)
	aliases := map[string]map[string]bool{}
	addAlias := func(original, handler string) {
		if aliases[original] == nil {
			aliases[original] = map[string]bool{}
		}
		aliases[original][handler] = true
	}
	for original, handler := range j.HandlerAliases {
		if !slices.Contains(workflow.FailureTargets(j.Document, original), handler) {
			return errors.New("failure handler alias does not match pinned workflow")
		}
		addAlias(original, handler)
	}
	// Branch-local alias maps are not task-global live bindings. Audited
	// failure transitions retain every historical alias solely for replay.
	for _, ev := range j.History {
		if (ev.Kind == "transition" || ev.Kind == "branch-transition") && strings.HasPrefix(ev.Cause, "failure ") && slices.Contains(workflow.FailureTargets(j.Document, string(ev.From)), string(ev.To)) {
			addAlias(string(ev.From), string(ev.To))
		}
	}
	for original, handlers := range aliases {
		s := j.Document.Stages[original]
		for _, a := range j.Evidence {
			if handlers[a.Stage] && a.Type == s.Output {
				a.Stage = original
				evidence = append(evidence, a)
			}
		}
	}
	for _, r := range j.GateRecords {
		if err := workflow.Replay(r, evidence); err != nil {
			return err
		}
	}
	return nil
}

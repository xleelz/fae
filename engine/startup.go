package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// ParkProviderUnavailable parks a task whose binding exhausted agent start.
const ParkProviderUnavailable = "ProviderUnavailable"

// AgentStartAttempts is the Appendix A agent-start bound: the starts one
// binding gets in one turn before it is charged Unavailable (FAE-RT-004).
// Prophet's operator turns use the same bound.
const AgentStartAttempts = 3

// ClassLaterReviewerNotStarted is a review dispatch whose later session
// provably never started (the transport's non-execution proof, or the
// bridge's provider-not-started) after its earlier sessions ran, with the
// processes retired and the tree unchanged (FAE-RT-004, FAE-WF-033). The
// session that failed never ran, so, like a first session's startup-retry,
// it charges no attempt; the result carries the reviews that ran so the
// engine can check them and name them.
const ClassLaterReviewerNotStarted = "later-reviewer-not-started"

// AgentStartCountKey is the durable counter (FAE-ROB-017) of one binding's
// failed agent starts in the current turn at stage: `startup-recovery:` then
// the stage, then the provider and model, the identity provider health
// charges. Every binding a turn tries has its own 3 starts (FAE-RT-004), so a
// review dispatch whose reviewers run on different bindings charges a binding
// Unavailable only after 3 failed starts of its own.
func AgentStartCountKey(stage string, b workflow.Binding) string {
	return turnStartupKeyPrefix + stage + ":" + b.Provider + "/" + b.Model
}

// clearStageAgentStarts forgets every binding's failed starts at stage, and
// the per-stage counter older daemons kept: a provider failover or a binding
// refusal moves the turn to another route with its full start bound.
func clearStageAgentStarts(j *Job, stage string) {
	delete(j.Failures, turnStartupKeyPrefix+stage)
	for key := range j.Failures {
		if strings.HasPrefix(key, turnStartupKeyPrefix+stage+":") {
			delete(j.Failures, key)
		}
	}
}

// stageAgentBindings lists, in declaration order without repeats, the
// bindings the roles a stage dispatches declare.
func stageAgentBindings(j Job, stage string) []workflow.Binding {
	s := j.Document.Stages[stage]
	roles := []string{s.Role}
	if s.Primitive == workflow.Review && len(s.Reviewers) > 0 {
		roles = s.Reviewers
	}
	var bindings []workflow.Binding
	for _, role := range roles {
		for _, b := range j.roleRoutes(role) {
			if !slices.Contains(bindings, b) {
				bindings = append(bindings, b)
			}
		}
	}
	return bindings
}

// StageAgentStartBound is the agent starts one turn at stage gets over every
// binding: AgentStartAttempts for each binding the stage's roles declare
// (FAE-RT-004). The per-binding count re-arms at each exhaustion, since the
// park it decides is re-decided once the binding is eligible again (another
// task's ok heals it at once); this total is not, so a turn whose starts
// keep failing ends once every declared binding has had its starts, whatever
// healed in between.
func StageAgentStartBound(j Job, stage string) int {
	return AgentStartAttempts * max(1, len(stageAgentBindings(j, stage)))
}

func formatStageAgentStartsExhausted(j Job, stage string, n, bound int, routeName, detail, reviewsSuffix string) string {
	var names []string
	for _, b := range stageAgentBindings(j, stage) {
		names = append(names, routeRefusalName(b))
	}
	return fmt.Sprintf("%s: %d agent starts failed in one turn, reaching the turn's bound of %d (%d per binding over %s); the last, on %s: %s", stage, n, bound, AgentStartAttempts, strings.Join(names, ", "), routeName, detail) + reviewsSuffix
}

func stopStageAgentStarts(j *Job, stage string, n, bound int, events *[]Event) {
	routeName, detail, reviewsSuffix := "", "", ""
	prefix := ParkProviderUnavailable + ": startup exhausted on "
	for i := len(j.History) - 1; i >= 0; i-- {
		e := j.History[i]
		if e.Stage == stage && e.Kind == "park" && strings.HasPrefix(e.Cause, prefix) {
			rest := strings.TrimPrefix(e.Cause, prefix)
			routeName = rest
			if idx := strings.Index(rest, " [after "); idx >= 0 {
				routeName, reviewsSuffix = rest[:idx], rest[idx:]
			}
			for k := i - 1; k >= 0; k-- {
				if j.History[k].Kind == "execution-diagnostic" && j.History[k].DispatchID == e.DispatchID {
					detail = bounded(j.History[k].Cause)
					break
				}
			}
			break
		}
	}
	stop(j, domain.ProviderUnavailable, formatStageAgentStartsExhausted(*j, stage, n, bound, routeName, detail, reviewsSuffix), events)
}

// completeStartupRetry records a failed agent start. Nothing of the session
// that failed ran, so it is refunded: the dispatch spends no task attempt,
// stage visit or test run (FAE-RT-004, operator ruling 2026-10-29). That holds
// for a dispatch's first session (startup-retry) and for a later review
// session after earlier ones ran (ClassLaterReviewerNotStarted): those
// reviews are not recorded (a partial collection never stands for the stage),
// and the stage's next dispatch runs every reviewer again in fresh sessions,
// so no reviewer sees another's output (FAE-WF-033). The start counts toward
// the agent-start bound of the binding of the session that failed to start;
// its third failed start in the turn parks ProviderUnavailable naming that
// binding, which the daemon charges Unavailable. It counts too toward the
// turn's total over every binding (StageAgentStartBound), which no park,
// refusal, failover or healing re-arms: once the turn has spent that total,
// no further start is launched and the turn escalates ProviderUnavailable
// naming the bindings. Those bounds, and the route rotation and
// route-exhaustion clock after them, keep the relaunches finite.
// Sequence is deliberately retained for fresh IDs.
func completeStartupRetry(input, j Job, d Dispatch, r Result, events []Event) (Job, []Event, error) {
	ran := len(r.Reviews)
	later := r.Class == ClassLaterReviewerNotStarted
	if j.ExecutionKind != "live" || r.ExecutionKind != "live" || (d.Config.Primitive != workflow.AgentTurn && d.Config.Primitive != workflow.Review) || r.NewTree != "" || r.NewBase != "" || len(r.Output) != 0 || later != (ran > 0) || later && (d.Config.Primitive != workflow.Review || !reviewsBeforeFailure(d, r.Reviews)) {
		return input, nil, errors.New("invalid startup recovery")
	}
	route := d.Sessions[ran].Route
	key := AgentStartCountKey(d.Stage, route)
	j.Failures[key]++
	refund(&j, d)
	j.InFlight = nil
	total := turnStartsKeyPrefix + d.Stage
	j.Failures[total]++
	if j.Failures[key] >= AgentStartAttempts {
		// The caller records ProviderUnavailable against the session binding.
		// The park charges nothing; route selection then rotates or bounds it.
		delete(j.Failures, key)
		j.Parked = true
		j.Reason = ParkProviderUnavailable
		event(&j, Event{Kind: "park", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: ParkProviderUnavailable + ": startup exhausted on " + route.Provider + "/" + route.Model + reviewsRanCause(d, r.Reviews, false)}, &events)
		return j, events, nil
	}
	if n, bound := j.Failures[total], StageAgentStartBound(j, d.Stage); n >= bound {
		stop(&j, domain.ProviderUnavailable, formatStageAgentStartsExhausted(j, d.Stage, n, bound, routeRefusalName(route), bounded(r.Detail), reviewsRanCause(d, r.Reviews, false)), &events)
		return j, events, nil
	}
	event(&j, Event{Kind: "startup-retry", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "positive nonexecution proof and retired transport" + reviewsRanCause(d, r.Reviews, false)}, &events)
	return j, events, nil
}

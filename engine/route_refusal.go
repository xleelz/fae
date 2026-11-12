package engine

import (
	"errors"
	"fmt"
	"strings"

	"github.com/xleelz/fae/workflow"
)

// ClassBindingRefused is a turn the trusted bridge refused before its provider
// ran because it cannot run the session's binding as configured: a binding of
// a provider the bridge does not serve, an unsupported model or effort,
// missing credentials, or the bridge's own executable or timeout. Retrying the
// same binding cannot succeed until that configuration changes, so the turn
// moves to the role's next route (FAE-RT-010, FAE-WF-082). The refused session
// never ran, so no provider health is charged (FAE-RT-009) and no attempt is
// spent (FAE-RT-004), whether it was the dispatch's first session or a later
// review session after earlier ones ran.
const ClassBindingRefused = "binding-refused"

// RouteRefusal is a binding the bridge refused for this task. Route selection
// skips it while the drive's route generation is the one it was refused in:
// a new generation (the daemon restarted, the only way its bridge command or
// target environment changes) or an operator Resume or restart with
// information tries it again. A changed binding (another model or effort) is
// a different binding, so a workflow fix needs no reset.
type RouteRefusal struct {
	Binding    workflow.Binding `json:"binding"`
	Detail     string           `json:"detail"`
	Generation string           `json:"generation,omitempty"`
	DispatchID string           `json:"dispatch_id"`
}

// routeRefusalName names a binding in refusal causes and details.
func routeRefusalName(b workflow.Binding) string {
	name := b.Provider + "/" + b.Model
	if b.Effort != "" {
		name += " (effort " + b.Effort + ")"
	}
	return name
}

// refusedRoute returns the binding's refusal in the given route generation.
func (j Job) refusedRoute(b workflow.Binding, generation string) (RouteRefusal, bool) {
	for _, r := range j.RefusedRoutes {
		if r.Binding == b && r.Generation == generation {
			return r, true
		}
	}
	return RouteRefusal{}, false
}

// unrefusedRoutes keeps the role's routes, in order, that the bridge has not
// refused in this route generation.
func (j Job) unrefusedRoutes(routes []workflow.Binding, generation string) []workflow.Binding {
	kept := []workflow.Binding{}
	for _, b := range routes {
		if _, refused := j.refusedRoute(b, generation); !refused {
			kept = append(kept, b)
		}
	}
	return kept
}

// refusedAtStage reports whether the bridge refused, in the given route
// generation, a route of a role the task's current stage dispatches: what
// makes a park for want of a route one Resume can act on (FAE-RT-010).
// Refusals of an earlier generation no longer apply.
func (j Job) refusedAtStage(generation string) bool {
	s, ok := j.Document.Stages[string(j.State)]
	if !ok {
		return false
	}
	roles := []string{s.Role}
	if s.Primitive == workflow.Review && len(s.Reviewers) > 0 {
		roles = s.Reviewers
	}
	for _, role := range roles {
		for _, b := range j.roleRoutes(role) {
			if _, refused := j.refusedRoute(b, generation); refused {
				return true
			}
		}
	}
	return false
}

// routeNeverLaunched reports a result whose failing session provably ran no
// provider: the bridge refused its binding, its agent never started, host
// admission deferred it before launch, it was refused before launch, or stale
// residue refused the dispatch. The route-exhaustion clock of a park for want
// of a route (FAE-RT-010; Appendix B ProviderUnavailable: "no route could
// launch") keeps running across such a dispatch, whichever binding it tried
// and whatever the refusal said: a daemon restart retries every refused
// binding, and a refusal that recurs there is what the bound exists to
// escalate. Review sessions that ran before the failing one don't stop the
// clock: the stage still can't complete without the session that never
// launched, and its next dispatch runs them again uncharged. Any other result
// (the failing session's provider ran, or may have) clears the clock.
func routeNeverLaunched(r Result) bool {
	switch r.Class {
	case ClassBindingRefused, "startup-retry", ClassLaterReviewerNotStarted, "infrastructure-deferred", ClassStaleResidue, ClassRefusedBeforeLaunch, ClassQuotaCutoff:
		return true
	}
	return false
}

// routeRefusalsDetail lists the refusals of the given routes, bounded.
func (j Job) routeRefusalsDetail(routes []workflow.Binding, generation string) string {
	var parts []string
	for _, b := range routes {
		if r, refused := j.refusedRoute(b, generation); refused {
			parts = append(parts, routeRefusalName(b)+": "+r.Detail)
		}
	}
	return bounded(strings.Join(parts, "; "))
}

// ParkRoutesUnavailable is the reason of a task parked because its role has
// no eligible route (FAE-RT-010): every route is unhealthy, or refused by the
// bridge in this route generation, or both. When the bridge refused every
// route, the reason goes on to name the refusals (routesRefusedReason).
const ParkRoutesUnavailable = "all-routes-unavailable"

// routesRefusedParkPrefix starts the reason of a park whose every route the
// bridge refused.
const routesRefusedParkPrefix = ParkRoutesUnavailable + ": every route for role "

// RoutesUnavailableReason reports whether reason is a no-eligible-route park's
// (ParkRoutesUnavailable), the form naming the bridge's refusals included.
func RoutesUnavailableReason(reason string) bool {
	return reason == ParkRoutesUnavailable || routesRefusedPark(reason)
}

// routesRefusedPark reports a park whose every route the bridge refused.
func routesRefusedPark(reason string) bool {
	return strings.HasPrefix(reason, routesRefusedParkPrefix)
}

// routesRefusedRemedy is what the operator can change when the bridge refused
// every route of a role.
const routesRefusedRemedy = "fix this instance's bridge command or provider credentials, or the role's bindings"

// routesRefusedReason is the park reason of a role whose every route the
// bridge refused in this route generation: the refusals and what the operator
// can change. No wait within the generation heals a refused binding, but the
// park resumes the task in place, charging nothing, when the daemon restarts
// (a new route generation) or the operator resumes it; past the
// route-exhaustion bound it escalates ProviderUnavailable (FAE-RT-010,
// Appendix B).
func routesRefusedReason(j Job, role string, routes []workflow.Binding, generation string) string {
	return fmt.Sprintf("%s%s was refused by the bridge before its provider ran: %s; %s, then restart the daemon or resume the task to try every route again at this stage", routesRefusedParkPrefix, role, j.routeRefusalsDetail(routes, generation), routesRefusedRemedy)
}

// dispatchRoute reports whether a session of the dispatch ran the binding.
func dispatchRoute(d Dispatch, b workflow.Binding) bool {
	for _, s := range d.Sessions {
		if s.Route == b {
			return true
		}
	}
	return false
}

// reviewsBeforeFailure reports whether a failed review dispatch's collected
// reviews are those of its first sessions, in order, short of the session
// that failed: what the executor returns when a later session fails.
func reviewsBeforeFailure(d Dispatch, reviews []ReviewResult) bool {
	if len(reviews) >= len(d.Sessions) {
		return false
	}
	for i, review := range reviews {
		if review.Session != d.Sessions[i].ID || review.Role != d.Sessions[i].Role {
			return false
		}
	}
	return true
}

// reviewsRanCause notes, in an event, the review sessions that ran before a
// later session failed. Their reviews are not recorded and the stage runs
// every reviewer again; charged says whether the dispatch keeps its attempt
// (a provider failover: the failing session's provider ran) or not (the
// failing session never ran, FAE-RT-004).
func reviewsRanCause(d Dispatch, reviews []ReviewResult, charged bool) string {
	if len(reviews) == 0 {
		return ""
	}
	sessions := make([]string, len(reviews))
	for i, review := range reviews {
		sessions[i] = review.Session + " (" + review.Role + ")"
	}
	ran := strings.Join(sessions, ", ")
	if len(ran) > 512 {
		ran = ran[:512] + "..."
	}
	charge := "the session that failed never ran, so no attempt is charged"
	if charged {
		charge = "the attempt stays charged"
	}
	return fmt.Sprintf(" [after %d of %d review sessions ran: %s; %s and the stage runs every reviewer again]", len(reviews), len(d.Sessions), ran, charge)
}

// completeBindingRefused records the refused binding and releases the turn
// at its stage: the next selection takes the role's next route, and no later
// dispatch of this route generation selects the binding. Each refusal adds
// one such binding, so a turn moves past at most as many refused bindings as
// the workflow declares, and the route-exhaustion clock keeps running
// (routeNeverLaunched).
//
// The refused session never ran, so the dispatch charges no attempt, visit or
// failover rotation, and the next route gets the turn's full agent-start
// bound (FAE-RT-004, operator ruling 2026-10-29). That holds too when a
// review dispatch's earlier sessions ran first: their reviews are not
// recorded as evidence (a partial collection never stands for the stage), and
// the stage's next dispatch runs every reviewer again in fresh sessions
// (FAE-WF-033).
func completeBindingRefused(input, j Job, d Dispatch, r Result, events []Event) (Job, []Event, error) {
	ran := len(r.Reviews)
	if j.ExecutionKind != "live" || r.ExecutionKind != "live" || (d.Config.Primitive != workflow.AgentTurn && d.Config.Primitive != workflow.Review) || len(r.Output) != 0 || r.NewTree != "" || r.NewBase != "" || r.RefusedRoute == nil || !reviewsBeforeFailure(d, r.Reviews) || (ran > 0 && d.Config.Primitive != workflow.Review) || d.Sessions[ran].Route != *r.RefusedRoute {
		return input, nil, errors.New("invalid binding refusal")
	}
	route := *r.RefusedRoute
	// Refusals of earlier generations no longer apply; keep one per binding.
	kept := []RouteRefusal{}
	for _, old := range j.RefusedRoutes {
		if old.Generation == d.RouteGeneration && old.Binding != route {
			kept = append(kept, old)
		}
	}
	detail := bounded(r.Detail)
	j.RefusedRoutes = append(kept, RouteRefusal{Binding: route, Detail: detail, Generation: d.RouteGeneration, DispatchID: d.ID})
	refund(&j, d)
	// The turn moves to another route with its full agent-start bound.
	clearStageAgentStarts(&j, d.Stage)
	j.InFlight = nil
	event(&j, Event{Kind: "route-refused", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: bounded(routeRefusalName(route)+": "+detail) + reviewsRanCause(d, r.Reviews, false)}, &events)
	return j, events, nil
}

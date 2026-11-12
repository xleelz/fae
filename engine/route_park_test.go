package engine_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// refuseEveryRoute has the bridge refuse the task's four routes in order,
// each with its own detail, and returns the park the next Prepare decides
// with its events.
func (x *turnBoundsTask) refuseEveryRoute(details [4]string) (engine.Job, []engine.Event) {
	x.t.Helper()
	for i, model := range []string{"one", "two", "three", "four"} {
		d := x.dispatch()
		if d.Sessions[0].Route.Model != model {
			x.t.Fatalf("refused route reselected: %+v, want %s", d.Sessions[0].Route, model)
		}
		x.refuseBinding(d, details[i])
	}
	j, d, events, err := engine.PrepareWith(x.j, x.options)
	if err != nil || d != nil || !j.Parked || !engine.RoutesUnavailableReason(j.Reason) {
		x.t.Fatalf("every route refused: %v dispatch %v parked=%v %q", err, d != nil, j.Parked, j.Reason)
	}
	return j, events
}

// FAE-RT-010: the refused park's reason is cut to its bound on a character
// boundary and is valid UTF-8, so the reason the daemon stores (as JSON, which
// turns each byte of invalid UTF-8 into U+FFFD) reads back exactly as the
// engine computes it. Each later round then decides the same park again with
// no event: a reason cut inside a multibyte character, or a refusal detail
// carrying invalid bytes, must not re-park with an event (and a plane commit
// and a drive wake-up) every round until the route-exhaustion bound.
func TestRefusedParkReasonReadsBackAsComputed(t *testing.T) {
	// The joined refusals are "p/one: <d1>; p/two: <d2>; ..."; d2 puts a
	// two-byte character across byte 1024 of the join.
	d1 := strings.Repeat("a", 600)
	prefix := "p/one: " + d1 + "; p/two: "
	d2 := strings.Repeat("b", 1024-len(prefix)-1) + strings.Repeat("é", 50)
	if joined := prefix + d2; utf8.RuneStart(joined[1024]) {
		t.Fatalf("fixture does not split a character at byte 1024")
	}
	for name, details := range map[string][4]string{
		"character across the bound": {d1, d2, "refused", "refused"},
		"invalid bytes in a detail":  {"bridge said \xff\xfe\xfd", "\xc3", "refused \xe2\x82", "refused"},
	} {
		x := newTurnBoundsTask(t)
		x.options.RouteGeneration = "daemon-1"
		j, events := x.refuseEveryRoute(details)
		if len(events) != 1 || events[0].Kind != "park" || events[0].Cause != j.Reason {
			t.Fatalf("%s: first park events %+v", name, events)
		}
		// The daemon persists the park as JSON and decides each later round
		// from the stored snapshot; a round with no event persists nothing.
		stored := roundTrip(t, j)
		for round := range 3 {
			again, d, events, err := engine.PrepareWith(stored, x.options)
			if err != nil || d != nil || len(events) != 0 || !again.Parked || again.Reason != stored.Reason {
				t.Fatalf("%s: round %d re-parked: %v dispatch %v events %+v", name, round, err, d != nil, events)
			}
		}
		if stored.Reason != j.Reason || !utf8.ValidString(j.Reason) || len(j.Reason) > 2048 {
			t.Fatalf("%s: park reason is not valid UTF-8, unbounded (%d bytes) or stored differently:\n%q\n%q", name, len(j.Reason), j.Reason, stored.Reason)
		}
	}
}

// FAE-RT-010, Appendix B ProviderUnavailable ("no route could launch after
// all bindings were tried and parks exceeded the route-exhaustion bound"):
// the route-exhaustion clock runs from the first no-route park until a route
// actually runs. A daemon restart (a new route generation) retries the
// refused bindings, but a retry the bridge refuses again launched nothing, so
// the clock keeps counting: a refusal that recurs at every start escalates
// once the bound has passed since the first park, however often the daemon
// restarts. A start failure (no provider ran) keeps the clock too; a turn
// whose provider ran clears it. The escalation keeps the clock (the
// daemon's route-exhaustion alert dates the episode from it) until a restart
// with information starts a new episode.
func TestRouteExhaustionClockRunsUntilARouteRuns(t *testing.T) {
	refused := [4]string{"no credentials", "no credentials", "no credentials", "no credentials"}
	x := newTurnBoundsTask(t)
	x.options.RouteGeneration = "daemon-1"
	parked, _ := x.refuseEveryRoute(refused)
	attempts := parked.Attempts
	// 25 minutes parked; the daemon restarts and the refusals recur.
	since := time.Now().UTC().Add(-25 * time.Minute)
	parked.RoutesUnavailableSince = since
	x.j = roundTrip(t, parked)
	x.options.RouteGeneration = "daemon-2"
	if d := x.dispatch(); d.Sessions[0].Route.Model != "one" || !x.j.RoutesUnavailableSince.Equal(since) {
		t.Fatalf("a new generation's retry restarted the clock: route %+v since %v, want %v", d.Sessions[0].Route, x.j.RoutesUnavailableSince, since)
	}
	x.refuseBinding(x.j.InFlight, "no credentials")
	for range 3 {
		x.refuseBinding(x.dispatch(), "no credentials")
	}
	j, d, events, err := engine.PrepareWith(x.j, x.options)
	if err != nil || d != nil || !j.Parked || len(events) != 1 || events[0].Kind != "park" || !j.RoutesUnavailableSince.Equal(since) || j.Attempts != attempts {
		t.Fatalf("recurring refusal after a restart: %v dispatch %v parked=%v since %v (want %v) events %+v", err, d != nil, j.Parked, j.RoutesUnavailableSince, since, events)
	}
	// Another restart 10 minutes later (35 since the first park): the
	// retries are refused again, and the park escalates.
	since = since.Add(-10 * time.Minute)
	j.RoutesUnavailableSince = since
	x.j = roundTrip(t, j)
	x.options.RouteGeneration = "daemon-3"
	for range 4 {
		x.refuseBinding(x.dispatch(), "no credentials")
	}
	j, d, _, err = engine.PrepareWith(x.j, x.options)
	if err != nil || d != nil || !j.Escalated(domain.ProviderUnavailable) || j.Attempts != attempts {
		t.Fatalf("a refusal recurring across restarts never escalated: %v dispatch %v %s %q", err, d != nil, j.State, j.Reason)
	}
	if !strings.Contains(j.EscalationDetail, "since "+since.Format(time.RFC3339)) || !j.RoutesUnavailableSince.Equal(since) {
		t.Fatalf("escalation does not date the episode: %q since %v", j.EscalationDetail, j.RoutesUnavailableSince)
	}
	x.j = roundTrip(t, j)
	x.control(engine.Control{Method: "RestartWithInformation", Information: "credentials supplied"})
	if !x.j.RoutesUnavailableSince.IsZero() {
		t.Fatalf("restart with information kept the old episode's clock: %v", x.j.RoutesUnavailableSince)
	}

	// A start failure launched nothing and keeps the clock; a turn whose
	// provider ran (here rate-limited) clears it.
	x = newTurnBoundsTask(t)
	x.options.RouteGeneration = "daemon-1"
	parked, _ = x.refuseEveryRoute(refused)
	since = parked.RoutesUnavailableSince
	x.j = roundTrip(t, parked)
	x.options.RouteGeneration = "daemon-2"
	x.complete(x.dispatch(), "startup-retry")
	if !x.j.RoutesUnavailableSince.Equal(since) {
		t.Fatalf("a start failure cleared the clock: %v", x.j.RoutesUnavailableSince)
	}
	x.complete(x.dispatch(), engine.ClassProviderFailover)
	if !x.j.RoutesUnavailableSince.IsZero() {
		t.Fatalf("a turn whose provider ran kept the clock: %v", x.j.RoutesUnavailableSince)
	}

	// A later reviewer's refusal after an earlier reviewer of the dispatch
	// ran: the refused session never launched and the stage can't complete
	// without it, so the clock keeps running (a refusal that recurs at every
	// daemon start still escalates). A later reviewer's provider failover
	// launched that provider, so it clears the clock.
	j, options := laterReviewerTask(t, map[string]bool{"m1": true, "c1": true, "m2": true})
	j, review := laterReviewerDispatch(t, j, options)
	since = time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	j.RoutesUnavailableSince = since
	refusedRoute := review.Sessions[1].Route
	ran := engine.ReviewResult{Session: review.Sessions[0].ID, Role: "a", Output: json.RawMessage(`{"approved":true}`)}
	later := engine.Result{ExecutionKind: "live", DispatchID: review.ID, TaskID: review.TaskID, Tree: review.Tree, Session: review.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: engine.ClassBindingRefused, Detail: "refused", RefusedRoute: &refusedRoute, Reviews: []engine.ReviewResult{ran}}
	next, _, err := engine.Complete(j, review.ID, later)
	if err != nil || !next.RoutesUnavailableSince.Equal(since) {
		t.Fatalf("a later reviewer's refusal cleared the clock: %v %v", err, next.RoutesUnavailableSince)
	}
	later.Class, later.RefusedRoute, later.Detail = engine.ClassProviderFailover, nil, "rate-limited: 429"
	next, _, err = engine.Complete(j, review.ID, later)
	if err != nil || !next.RoutesUnavailableSince.IsZero() {
		t.Fatalf("a later reviewer's failover kept the clock: %v %v", err, next.RoutesUnavailableSince)
	}
}

// FAE-RT-010: Resume accepts a park for want of a route only when the bridge
// refused one of the stage's routes in the caller's current route generation;
// it forgets the refusals and restarts the route-exhaustion clock. A park on
// unhealthy routes alone is refused, also when the task still carries a
// refusal from an earlier daemon start: that refusal no longer applies, and
// health heals by decay.
func TestResumeNeedsARefusalOfTheCurrentRouteGeneration(t *testing.T) {
	// The bridge refused route one under the first daemon start; after a
	// restart every route is unhealthy.
	x := newTurnBoundsTask(t)
	x.options.RouteGeneration = "daemon-1"
	x.refuseBinding(x.dispatch(), "no credentials")
	x.options.RouteGeneration = "daemon-2"
	for model := range x.healthy {
		x.healthy[model] = false
	}
	j, d, _, err := engine.PrepareWith(x.j, x.options)
	if err != nil || d != nil || !j.Parked || j.Reason != engine.ParkRoutesUnavailable || len(j.RefusedRoutes) != 1 {
		t.Fatalf("health-only park: %v dispatch %v parked=%v %q refused %+v", err, d != nil, j.Parked, j.Reason, j.RefusedRoutes)
	}
	j = roundTrip(t, j)
	if _, _, err := engine.ApplyControl(j, engine.Control{Method: "Resume", Reason: "r", RouteGeneration: "daemon-2"}, time.Now()); err == nil {
		t.Fatal("resume accepted a park on unhealthy routes whose only refusal is from an earlier daemon start")
	}
	// The same stored park in the generation that refused route one: a
	// park on refused and unhealthy routes, which Resume accepts.
	resumed, _, err := engine.ApplyControl(j, engine.Control{Method: "Resume", Reason: "r", RouteGeneration: "daemon-1"}, time.Now())
	if err != nil || resumed.Parked || len(resumed.RefusedRoutes) != 0 || !resumed.RoutesUnavailableSince.IsZero() {
		t.Fatalf("resume of a refused and unhealthy park: %v parked=%v refused %+v since %v", err, resumed.Parked, resumed.RefusedRoutes, resumed.RoutesUnavailableSince)
	}
}

// FAE-RT-004: the agent-start bound counts one route's starts. A provider
// failover moves the turn to the next route with its full 3 starts, so a
// binding is never charged Unavailable for fewer than 3 failed starts of its
// own; the turn's one failover rotation stays spent.
func TestProviderFailoverGivesTheNextRouteFreshStarts(t *testing.T) {
	x := newTurnBoundsTask(t)
	x.run("startup-retry", "startup-retry")
	d := x.dispatch()
	if d.Sessions[0].Route.Model != "one" {
		t.Fatalf("third start: %+v", d.Sessions[0].Route)
	}
	x.complete(d, engine.ClassProviderFailover)
	if n := agentStarts(x.j, "intake"); n != 0 || x.j.Failures["failover:intake"] != 1 {
		t.Fatalf("failover carried the failed route's starts (%d) or spent no rotation: %v", n, x.j.Failures)
	}
	from := len(x.j.History)
	x.run("startup-retry", "startup-retry")
	if x.j.Parked || x.events("park", from) != 0 {
		t.Fatalf("the next route parked before its third start: %q", x.j.Reason)
	}
	if d := x.dispatch(); d.Sessions[0].Route.Model != "two" {
		t.Fatalf("third start of the next route: %+v", d.Sessions[0].Route)
	}
}

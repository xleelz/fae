package engine_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// refuseBinding collects d as the bridge's refusal of its first session's
// binding before the provider ran (the executor's ClassBindingRefused), and
// round-trips the row.
func (x *turnBoundsTask) refuseBinding(d *engine.Dispatch, detail string) []engine.Event {
	x.t.Helper()
	route := d.Sessions[0].Route
	r := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: engine.ClassBindingRefused, Detail: detail, RefusedRoute: &route}
	j, events, err := engine.Complete(x.j, d.ID, r)
	if err != nil {
		x.t.Fatal(err)
	}
	x.j = roundTrip(x.t, j)
	return events
}

// FAE-RT-010, FAE-WF-082, FAE-RT-009, FAE-RT-004: a binding the bridge
// refuses before its provider runs (here an effort Claude Code lacks) fails
// over to the role's next route in the same turn. It charges no attempt and
// no failover rotation, records no stage failure, and is never selected
// again for the task in its route generation, at this stage or another,
// although its provider health is untouched. A new generation (a daemon
// restart) tries it again.
func TestBindingRefusalFailsOverToTheNextRoute(t *testing.T) {
	x := newTurnBoundsTask(t)
	x.options.RouteGeneration = "daemon-1"
	first := x.dispatch()
	if first.Sessions[0].Route.Model != "one" || first.RouteGeneration != "daemon-1" {
		t.Fatalf("first dispatch: route %+v generation %q", first.Sessions[0].Route, first.RouteGeneration)
	}
	attempts, from := x.j.Attempts, len(x.j.History)
	events := x.refuseBinding(first, `bridge refused the binding before the provider ran: unsupported Claude effort "minimal"`)
	last := events[len(events)-1]
	if x.events("route-refused", from) != 1 || last.Kind != "route-refused" || !strings.HasPrefix(last.Cause, "p/one: ") || !strings.Contains(last.Cause, `unsupported Claude effort "minimal"`) {
		t.Fatalf("refusal events %+v", events)
	}
	if x.j.InFlight != nil || x.j.Parked || x.j.State != "intake" || x.j.Attempts != attempts-1 || x.j.Counts["intake"] != 0 || x.j.Failures["failover:intake"] != 0 || len(x.j.RefusedRoutes) != 1 {
		t.Fatalf("refusal charged or held the turn: %s parked=%v attempts %d->%d failures %v refused %+v", x.j.State, x.j.Parked, attempts, x.j.Attempts, x.j.Failures, x.j.RefusedRoutes)
	}
	// The next route takes the turn, and its provider failover still has
	// the turn's rotation to spend.
	second := x.dispatch()
	if second.Sessions[0].Route.Model != "two" || second.Attempt != attempts {
		t.Fatalf("turn did not move to the next route: %+v attempt %d", second.Sessions[0].Route, second.Attempt)
	}
	x.complete(second, engine.ClassProviderFailover)
	third := x.dispatch()
	if third.Sessions[0].Route.Model != "three" || x.j.Failures["failover:intake"] != 1 {
		t.Fatalf("provider failover after the refusal: %+v failures %v", third.Sessions[0].Route, x.j.Failures)
	}
	x.complete(third, "success")
	if x.j.State == "intake" || x.events("failure", from) != 0 || x.events("route-refused", from) != 1 {
		t.Fatalf("turn did not complete on the next route: %s %q", x.j.State, x.j.Reason)
	}
	// The refused binding stays refused at the task's next stage; health
	// (here: healthy) does not re-admit it.
	x.healthy["two"] = true
	next := x.dispatch()
	if next.Stage == "intake" || next.Sessions[0].Route.Model != "two" {
		t.Fatalf("next stage %s selected %+v", next.Stage, next.Sessions[0].Route)
	}
	// A new route generation tries the refused binding again.
	x.complete(next, engine.ClassProviderFailover)
	x.options.RouteGeneration = "daemon-2"
	x.healthy["two"] = true
	if again := x.dispatch(); again.Sessions[0].Route.Model != "one" {
		t.Fatalf("new generation kept the refusal: %+v", again.Sessions[0].Route)
	}
}

// FAE-RT-010, Appendix B ProviderUnavailable: when the bridge has refused
// every route of the role, the task parks (all-routes-unavailable) with every
// refusal named and what the operator can change, charging no attempt and
// recording no stage failure. Within the route generation the park is decided
// again each round without an event or a dispatch, so it cannot busy-loop.
// It resumes in place, charging nothing, when the route generation changes (a
// daemon restart; the route-exhaustion clock keeps running until a route
// runs) or on Resume (which restarts the clock), and escalates
// ProviderUnavailable only past the route-exhaustion bound; a restart with
// information then clears the refusals. Scripted callers without a route
// selector park and resume alike.
func TestEveryRouteRefusedParksWithTheRefusals(t *testing.T) {
	for _, selector := range []bool{true, false} {
		x := newTurnBoundsTask(t)
		x.options.RouteGeneration = "daemon-1"
		if !selector {
			x.options.SelectRoute = nil
		}
		attempts, from := x.j.Attempts, len(x.j.History)
		var first *engine.Dispatch
		for _, model := range []string{"one", "two", "three", "four"} {
			d := x.dispatch()
			if d.Sessions[0].Route.Model != model {
				t.Fatalf("selector %v: refused route reselected: %+v, want %s", selector, d.Sessions[0].Route, model)
			}
			if first == nil {
				first = d
			}
			x.refuseBinding(d, "bridge refused the binding before the provider ran: this claude bridge does not serve \"p\" bindings")
		}
		j, d, events, err := engine.PrepareWith(x.j, x.options)
		if err != nil || d != nil || !j.Parked || string(j.State) != first.Stage || len(events) != 1 || events[0].Kind != "park" || events[0].Cause != j.Reason {
			t.Fatalf("selector %v: every route refused: %v dispatch %v %s parked=%v %q events %+v", selector, err, d != nil, j.State, j.Parked, j.Reason, events)
		}
		for _, want := range []string{"all-routes-unavailable: every route for role spec was refused by the bridge before its provider ran: ", "p/one: ", "p/four: ", "does not serve", "bridge command or provider credentials", "restart the daemon or resume the task"} {
			if !strings.Contains(j.Reason, want) {
				t.Fatalf("selector %v: park reason lacks %q: %s", selector, want, j.Reason)
			}
		}
		if !engine.RoutesUnavailableReason(j.Reason) || j.Attempts != attempts || x.events("failure", from) != 0 || j.RoutesUnavailableSince.IsZero() {
			t.Fatalf("selector %v: park charged or untyped: attempts %d->%d since %v", selector, attempts, j.Attempts, j.RoutesUnavailableSince)
		}
		parked := roundTrip(t, j)
		// Within the generation, every later round decides the same park
		// again: no event, no dispatch, nothing changed.
		for range 3 {
			again, d, events, err := engine.PrepareWith(parked, x.options)
			if err != nil || d != nil || len(events) != 0 || !again.Parked || again.Reason != parked.Reason || !again.RoutesUnavailableSince.Equal(parked.RoutesUnavailableSince) {
				t.Fatalf("selector %v: refused park re-decided with %v dispatch %v events %+v %q", selector, err, d != nil, events, again.Reason)
			}
		}
		// A new route generation (a daemon restart) resumes the task in
		// place: the first route takes the turn on the attempt the refused
		// dispatches never spent. The retry launched nothing yet, so the
		// route-exhaustion clock keeps running.
		x.j = parked
		x.options.RouteGeneration = "daemon-2"
		if d := x.dispatch(); d.Sessions[0].Route.Model != "one" || d.Stage != first.Stage || d.Attempt != first.Attempt || !x.j.RoutesUnavailableSince.Equal(parked.RoutesUnavailableSince) {
			t.Fatalf("selector %v: new generation did not resume in place: %+v stage %s attempt %d->%d", selector, d.Sessions[0].Route, d.Stage, first.Attempt, d.Attempt)
		}
		// Resume forgets the refusals of this generation and resumes in
		// place too.
		x.j = parked
		x.options.RouteGeneration = "daemon-1"
		x.control(engine.Control{Method: "Resume", RouteGeneration: "daemon-1"})
		if x.j.Parked || len(x.j.RefusedRoutes) != 0 || string(x.j.State) != first.Stage || x.j.Attempts != attempts || !x.j.RoutesUnavailableSince.IsZero() {
			t.Fatalf("selector %v: resume: %s parked=%v refused %+v attempts %d", selector, x.j.State, x.j.Parked, x.j.RefusedRoutes, x.j.Attempts)
		}
		if d := x.dispatch(); d.Sessions[0].Route.Model != "one" || d.Attempt != first.Attempt {
			t.Fatalf("selector %v: resume did not try the first route: %+v", selector, d.Sessions[0].Route)
		}
		// Past the route-exhaustion bound the park escalates
		// ProviderUnavailable, naming the refusals and the remedy.
		parked.RoutesUnavailableSince = time.Now().Add(-2 * engine.DefaultRouteExhaustion)
		j, d, _, err = engine.PrepareWith(parked, x.options)
		if err != nil || d != nil || !j.Escalated(domain.ProviderUnavailable) || j.Attempts != attempts {
			t.Fatalf("selector %v: past the bound: %v dispatch %v %s %q", selector, err, d != nil, j.State, j.Reason)
		}
		for _, want := range []string{"no eligible route for role spec since ", "refused by the bridge: p/one: ", "p/four: ", "every route was refused before its provider ran", "restart the task with information"} {
			if !strings.Contains(j.EscalationDetail, want) {
				t.Fatalf("selector %v: detail lacks %q: %s", selector, want, j.EscalationDetail)
			}
		}
		x.j = roundTrip(t, j)
		x.control(engine.Control{Method: "RestartWithInformation", Information: "credentials supplied"})
		if len(x.j.RefusedRoutes) != 0 {
			t.Fatalf("selector %v: restart kept the refusals %+v", selector, x.j.RefusedRoutes)
		}
		if d := x.dispatch(); d.Sessions[0].Route.Model != "one" {
			t.Fatalf("selector %v: restart did not try the first route: %+v", selector, d.Sessions[0].Route)
		}
	}
	// Resume refuses a route park the bridge refused nothing in: only
	// health, which heals by decay, keeps those routes.
	x := newTurnBoundsTask(t)
	for model := range x.healthy {
		x.healthy[model] = false
	}
	j, _, _, err := engine.PrepareWith(x.j, x.options) // Pending -> intake
	if err == nil {
		j, _, _, err = engine.PrepareWith(j, x.options)
	}
	if err != nil || !j.Parked || j.Reason != engine.ParkRoutesUnavailable {
		t.Fatalf("unhealthy routes: %v parked=%v %q", err, j.Parked, j.Reason)
	}
	if _, _, err := engine.ApplyControl(j, engine.Control{Method: "Resume"}, time.Now()); err == nil {
		t.Fatal("resume accepted a park with no refused route")
	}
}

// FAE-RT-004: a refusal moves the turn to the next route with the turn's
// full agent-start bound: the refused route's start failures are not
// carried to it.
func TestBindingRefusalGivesTheNextRouteFreshStarts(t *testing.T) {
	x := newTurnBoundsTask(t)
	x.run("startup-retry", "startup-retry")
	if agentStarts(x.j, "intake") != 2 {
		t.Fatalf("fixture starts %v", x.j.Failures)
	}
	x.refuseBinding(x.dispatch(), "bridge refused the binding before the provider ran: credentials missing")
	if n := agentStarts(x.j, "intake"); n != 0 {
		t.Fatalf("the refused route's start failures carried to the next route: %d", n)
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

// laterReviewerTask is a live Review dispatch of two reviewers whose first
// routes differ: a on claude/m1, b on codex/c1 then claude/m2.
func laterReviewerTask(t *testing.T, healthy map[string]bool) (engine.Job, engine.Options) {
	t.Helper()
	route := func(b ...workflow.Binding) []workflow.Binding { return b }
	approved := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "review", Path: []string{"approved"}}, {Op: "literal", Value: true}}}
	doc := workflow.Document{SchemaVersion: 1, ID: "later-reviewer", Start: "review", Limits: workflow.Limits{Attempts: 8, TestRuns: 2, ScopeWidens: 1},
		Roles: map[string]workflow.Role{
			"a": {Instructions: "review", Briefs: map[string]string{"default": "brief"}, Routes: route(workflow.Binding{Provider: "claude", Model: "m1"}), Scope: "task-bound", Cap: 1, Outputs: []string{"Verdict"}},
			"b": {Instructions: "review", Briefs: map[string]string{"default": "brief"}, Routes: route(workflow.Binding{Provider: "codex", Model: "c1"}, workflow.Binding{Provider: "claude", Model: "m2"}), Scope: "task-bound", Cap: 1, Outputs: []string{"Verdict"}},
		},
		Artifacts: map[string]workflow.ArtifactType{"Verdict": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["approved"],"properties":{"approved":{"type":"boolean"}}}`)}},
		Stages: map[string]workflow.Stage{
			"review": {Primitive: workflow.Review, Role: "a", Reviewers: []string{"a", "b"}, Output: "Verdict"},
			"gate":   {Primitive: workflow.Gate, Expr: &approved, Failure: "stop"},
			"stop":   {Primitive: workflow.Escalate},
		},
		Edges: []workflow.Edge{{From: "review", To: "gate"}, {From: "gate", To: "Done"}, {From: "gate", To: "stop"}},
	}
	if issues := workflow.Validate(doc); len(issues) != 0 {
		t.Fatal(issues)
	}
	j, err := engine.NewLive("later-reviewer", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	options := engine.Options{RouteGeneration: "daemon-1", SelectRoute: func(routes []workflow.Binding) (workflow.Binding, bool) {
		for _, r := range routes {
			if healthy[r.Model] {
				return r, true
			}
		}
		return workflow.Binding{}, false
	}}
	return j, options
}

// laterReviewerDispatch prepares the task's next review dispatch.
func laterReviewerDispatch(t *testing.T, j engine.Job, options engine.Options) (engine.Job, *engine.Dispatch) {
	t.Helper()
	for range 5 {
		next, d, _, err := engine.PrepareWith(j, options)
		if err != nil {
			t.Fatal(err)
		}
		j = roundTrip(t, next)
		if d != nil {
			return j, d
		}
	}
	t.Fatalf("no review dispatch: %s parked=%v %q", j.State, j.Parked, j.Reason)
	return j, nil
}

// FAE-RT-004 (operator ruling 2026-10-29), FAE-WF-033, FAE-RT-010: a later
// reviewer's binding refused after an earlier reviewer of the same dispatch
// ran is refunded like a first session's: the refused session never ran, so
// the dispatch charges no attempt or visit. A later reviewer's provider
// failover is charged: that provider ran. Either way the event names the
// review session that ran, the collected review is not recorded as the
// stage's evidence, the refused binding is still skipped, and the stage runs
// every reviewer again in fresh sessions.
func TestLaterReviewerRefusalIsRefundedAndFailoverStaysCharged(t *testing.T) {
	for _, class := range []string{engine.ClassBindingRefused, engine.ClassProviderFailover} {
		t.Run(class, func(t *testing.T) {
			healthy := map[string]bool{"m1": true, "c1": true, "m2": true}
			j, options := laterReviewerTask(t, healthy)
			j, d := laterReviewerDispatch(t, j, options)
			if len(d.Sessions) != 2 || d.Sessions[0].Route.Model != "m1" || d.Sessions[1].Route.Model != "c1" || j.Attempts != 1 {
				t.Fatalf("%s: first dispatch %+v attempts %d", class, d.Sessions, j.Attempts)
			}
			review := engine.ReviewResult{Session: d.Sessions[0].ID, Role: "a", Output: json.RawMessage(`{"approved":true}`)}
			refused := d.Sessions[1].Route
			r := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: class, Detail: "bridge refused the binding before the provider ran: this claude bridge does not serve \"codex\" bindings", Reviews: []engine.ReviewResult{review}}
			if class == engine.ClassBindingRefused {
				r.RefusedRoute = &refused
			} else {
				healthy["c1"] = false
				r.Detail = "rate-limited: 429"
			}
			next, events, err := engine.Complete(j, d.ID, r)
			if err != nil {
				t.Fatal(class, err)
			}
			j = roundTrip(t, next)
			charged, note := 0, "the session that failed never ran, so no attempt is charged"
			if class == engine.ClassProviderFailover {
				charged, note = 1, "the attempt stays charged"
			}
			if j.Attempts != charged || j.Counts["review"] != charged || j.InFlight != nil || j.Parked || len(j.Evidence) != 0 {
				t.Fatalf("%s: attempts %d visits %d evidence %d parked=%v, want %d charged and nothing recorded", class, j.Attempts, j.Counts["review"], len(j.Evidence), j.Parked, charged)
			}
			last := events[len(events)-1]
			if !strings.Contains(last.Cause, "after 1 of 2 review sessions ran: "+review.Session+" (a)") || !strings.Contains(last.Cause, note) {
				t.Fatalf("%s: event does not name the review that ran and its charge: %+v", class, last)
			}
			if class == engine.ClassBindingRefused && (len(j.RefusedRoutes) != 1 || j.RefusedRoutes[0].Binding != refused || j.Failures["failover:review"] != 0) {
				t.Fatalf("refusal not recorded, or a rotation spent: %+v %v", j.RefusedRoutes, j.Failures)
			}
			if class == engine.ClassProviderFailover && j.Failures["failover:review"] != 1 {
				t.Fatalf("failover rotation not spent: %v", j.Failures)
			}
			// The stage runs both reviewers again, b on its next route.
			j, again := laterReviewerDispatch(t, j, options)
			if again.Attempt != charged+1 || again.Visit != charged+1 || again.Sessions[0].Route.Model != "m1" || again.Sessions[1].Route.Model != "m2" {
				t.Fatalf("%s: redispatch %+v attempt %d visit %d", class, again.Sessions, again.Attempt, again.Visit)
			}
			reviews := []engine.ReviewResult{{Session: again.Sessions[0].ID, Role: "a", Output: json.RawMessage(`{"approved":true}`)}, {Session: again.Sessions[1].ID, Role: "b", Output: json.RawMessage(`{"approved":true}`)}}
			done, _, err := engine.Complete(j, again.ID, engine.Result{ExecutionKind: "live", DispatchID: again.ID, TaskID: again.TaskID, Tree: again.Tree, Session: again.Sessions[0].ID, ProducedAt: time.Now().UTC(), Reviews: reviews})
			if err != nil || len(done.Evidence) != 2 || done.Provenance[1].Model != "m2" {
				t.Fatalf("%s: second pass: %v evidence %d", class, err, len(done.Evidence))
			}
		})
	}
	// The first session's refusal: nothing ran, so the dispatch is refunded.
	healthy := map[string]bool{"m1": true, "c1": true, "m2": true}
	j, options := laterReviewerTask(t, healthy)
	j, d := laterReviewerDispatch(t, j, options)
	route := d.Sessions[0].Route
	next, _, err := engine.Complete(j, d.ID, engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: engine.ClassBindingRefused, Detail: "refused", RefusedRoute: &route})
	if err != nil || next.Attempts != 0 || next.Counts["review"] != 0 {
		t.Fatalf("first-session refusal: %v attempts %d visits %d", err, next.Attempts, next.Counts["review"])
	}
	// A refusal must name the binding of the session that failed: the one
	// after the collected reviews.
	for name, mutate := range map[string]func(*engine.Result){
		"earlier session's binding": func(r *engine.Result) { r.RefusedRoute = &d.Sessions[0].Route },
		"foreign review session":    func(r *engine.Result) { r.Reviews[0].Session = "other" },
		"every review collected": func(r *engine.Result) {
			r.Reviews = append(r.Reviews, engine.ReviewResult{Session: d.Sessions[1].ID, Role: "b", Output: json.RawMessage(`{"approved":true}`)})
		},
	} {
		refused := d.Sessions[1].Route
		r := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: engine.ClassBindingRefused, Detail: "refused", RefusedRoute: &refused, Reviews: []engine.ReviewResult{{Session: d.Sessions[0].ID, Role: "a", Output: json.RawMessage(`{"approved":true}`)}}}
		mutate(&r)
		if _, _, err := engine.Complete(j, d.ID, r); err == nil {
			t.Fatalf("%s: accepted an invalid later-session refusal", name)
		}
	}
}

// FAE-RT-010: refused bindings and unhealthy ones together leave no eligible
// route, which parks all-routes-unavailable as before (a throttled binding
// heals). Past the route-exhaustion bound, the ProviderUnavailable detail
// also names the refusals.
func TestRefusedAndUnhealthyRoutesPark(t *testing.T) {
	x := newTurnBoundsTask(t)
	for range 2 {
		x.refuseBinding(x.dispatch(), "bridge refused the binding before the provider ran: credentials missing")
	}
	x.healthy["three"], x.healthy["four"] = false, false
	j, d, _, err := engine.PrepareWith(x.j, x.options)
	if err != nil || d != nil || !j.Parked || j.Reason != "all-routes-unavailable" {
		t.Fatalf("refused and unhealthy routes: %v %s parked=%v %q", err, j.State, j.Parked, j.Reason)
	}
	j.RoutesUnavailableSince = time.Now().Add(-2 * engine.DefaultRouteExhaustion)
	j, _, _, err = engine.PrepareWith(j, x.options)
	if err != nil || !j.Escalated(domain.ProviderUnavailable) || !strings.Contains(j.EscalationDetail, "no eligible route for role spec") || !strings.Contains(j.EscalationDetail, "refused by the bridge: p/one: ") || !strings.Contains(j.EscalationDetail, "p/two: ") {
		t.Fatalf("route exhaustion: %v %s %q", err, j.State, j.EscalationDetail)
	}
}

// A binding refusal is valid only for a live agent turn, naming a binding a
// session of the dispatch ran, with nothing produced.
func TestBindingRefusalEnvelope(t *testing.T) {
	x := newTurnBoundsTask(t)
	d := x.dispatch()
	route := d.Sessions[0].Route
	other := workflow.Binding{Provider: "p", Model: "two"}
	base := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: engine.ClassBindingRefused, Detail: "refused", RefusedRoute: &route}
	for name, mutate := range map[string]func(*engine.Result){
		"no binding":      func(r *engine.Result) { r.RefusedRoute = nil },
		"foreign binding": func(r *engine.Result) { r.RefusedRoute = &other },
		"changed tree":    func(r *engine.Result) { r.NewTree = "edited" },
		"output":          func(r *engine.Result) { r.Output = []byte(`{"accepted":true}`) },
		"scripted":        func(r *engine.Result) { r.ExecutionKind = "scripted" },
	} {
		r := base
		mutate(&r)
		if _, _, err := engine.Complete(x.j, d.ID, r); err == nil {
			t.Fatalf("%s: accepted an invalid binding refusal", name)
		}
	}
}

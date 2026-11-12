package engine_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// refusedBeforeLaunchClass is engine.ClassRefusedBeforeLaunch, spelled out so
// the tests pin its wire value.
const refusedBeforeLaunchClass = "refused-before-launch"

// agentStarts is the failed agent starts the current turn at stage holds,
// over every binding (engine.AgentStartCountKey).
func agentStarts(j engine.Job, stage string) int {
	n := j.Failures["startup-recovery:"+stage]
	for key, count := range j.Failures {
		if strings.HasPrefix(key, "startup-recovery:"+stage+":") {
			n += count
		}
	}
	return n
}

// neverExecutedTestRunTask is a live task whose first stage is a test run.
func neverExecutedTestRunTask(t *testing.T) engine.Job {
	t.Helper()
	pass := workflow.Expr{Op: "and", Args: []workflow.Expr{
		{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "test", Path: []string{"exit_code"}}, {Op: "literal", Value: 0}}},
		{Op: "ge", Args: []workflow.Expr{{Op: "field", Stage: "test", Path: []string{"executed_count"}}, {Op: "literal", Value: 1}}}}}
	doc := workflow.Document{SchemaVersion: 1, ID: "never-executed-run", Start: "test", Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{},
		Artifacts: map[string]workflow.ArtifactType{"Report": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}},
		Stages: map[string]workflow.Stage{
			"test": {Primitive: workflow.HostCommand, Output: "Report", Command: []string{"make", "test"}, Weight: 1, TimeoutSeconds: 30, TestRun: true},
			"gate": {Primitive: workflow.Gate, Expr: &pass, Failure: "stop"},
			"stop": {Primitive: workflow.Escalate, Reason: "tests failed"},
		},
		Edges: []workflow.Edge{{From: "test", To: "gate"}, {From: "gate", To: "Done"}, {From: "gate", To: "stop"}}}
	if issues := workflow.Validate(doc); len(issues) != 0 {
		t.Fatal(issues)
	}
	j, err := engine.NewLive("never-executed-run", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// neverExecutedDispatch prepares j's next dispatch.
func neverExecutedDispatch(t *testing.T, j engine.Job) (engine.Job, *engine.Dispatch) {
	t.Helper()
	for range 5 {
		next, d, _, err := engine.PrepareWith(j, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		j = roundTrip(t, next)
		if d != nil {
			return j, d
		}
		if j.Parked || j.State == domain.Escalated || j.State.Terminal() {
			t.Fatalf("no dispatch: %s parked=%v %q", j.State, j.Parked, j.Reason)
		}
	}
	t.Fatal("no dispatch")
	return j, nil
}

// neverExecutedEvent is the first event of kind.
func neverExecutedEvent(events []engine.Event, kind string) engine.Event {
	for _, e := range events {
		if e.Kind == kind {
			return e
		}
	}
	return engine.Event{}
}

// neverExecutedComplete collects d with a result of class and detail.
func neverExecutedComplete(t *testing.T, j engine.Job, d *engine.Dispatch, class, detail string, output json.RawMessage) (engine.Job, []engine.Event) {
	t.Helper()
	r := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: class, Detail: detail, Output: output}
	next, events, err := engine.Complete(j, d.ID, r)
	if err != nil {
		t.Fatal(class, err)
	}
	return roundTrip(t, next), events
}

// FAE-RT-004, FAE-WF-074 (operator ruling 2026-10-29: a run never executed
// doesn't count towards the budget): a test run refused before it started
// (the daemon's scope refusal, an unresolvable task or a refused command)
// charges neither an attempt nor a test run, and is recorded as the stage's
// failure. Refusals before launch at a stage count toward SameGate as one
// reason, whatever their detail, so the fourth in a row escalates SameGate:
// the refund leaves the attempt limit nothing to bound. A run that does start
// restarts the count and is charged as usual.
func TestRefusedBeforeLaunchChargesNoAttemptOrTestRun(t *testing.T) {
	for name, detail := range map[string]func(int) string{
		"identical refusals": func(int) string { return "task scope refused: the task has no RedReport naming its RED identity" },
		"varying refusals":   func(n int) string { return fmt.Sprintf("worktree for task never-executed-run unreadable at try %d", n) },
	} {
		t.Run(name, func(t *testing.T) {
			j := neverExecutedTestRunTask(t)
			for n := 1; n <= engine.SameGateRepeats+1; n++ {
				var d *engine.Dispatch
				j, d = neverExecutedDispatch(t, j)
				if d.Attempt != 1 || !d.AttemptCharged || j.TestRuns != 1 {
					t.Fatalf("refusal %d: dispatch attempt %d charged=%v test runs %d", n, d.Attempt, d.AttemptCharged, j.TestRuns)
				}
				var events []engine.Event
				j, events = neverExecutedComplete(t, j, d, refusedBeforeLaunchClass, detail(n), nil)
				if j.Attempts != 0 || j.TestRuns != 0 || j.Counts["test"] != 0 || j.InFlight != nil {
					t.Fatalf("refusal %d charged: attempts %d test runs %d visits %d", n, j.Attempts, j.TestRuns, j.Counts["test"])
				}
				if failure := neverExecutedEvent(events, "failure"); failure.Cause != "RefusedBeforeLaunch: "+detail(n) {
					t.Fatalf("refusal %d: events %+v", n, events)
				}
				if n <= engine.SameGateRepeats {
					if j.State != "test" || j.Parked {
						t.Fatalf("refusal %d ended the stage: %s parked=%v %q", n, j.State, j.Parked, j.Reason)
					}
					continue
				}
				if !j.Escalated(domain.SameGate) || !strings.Contains(j.EscalationDetail, "test failed 4 times for the same reason: refused before launch") || !strings.Contains(j.EscalationDetail, detail(n)) {
					t.Fatalf("fourth refusal: %s kind %s %q", j.State, j.EscalationKind, j.EscalationDetail)
				}
			}
		})
	}
	// Two refusals, then the run starts: charged once, and the refusals'
	// same-gate count restarts.
	j := neverExecutedTestRunTask(t)
	for range 2 {
		var d *engine.Dispatch
		j, d = neverExecutedDispatch(t, j)
		j, _ = neverExecutedComplete(t, j, d, refusedBeforeLaunchClass, "plane busy", nil)
	}
	j, d := neverExecutedDispatch(t, j)
	j, _ = neverExecutedComplete(t, j, d, "", "", json.RawMessage(`{"exit_code":0,"executed_count":1,"fresh_build":true}`))
	if j.Attempts != 1 || j.TestRuns != 1 {
		t.Fatalf("the run that started: attempts %d test runs %d", j.Attempts, j.TestRuns)
	}
	for key := range j.Failures {
		if strings.HasPrefix(key, "same-gate:test:") {
			t.Fatalf("a run that started kept the refusals' count: %v", j.Failures)
		}
	}
	// The class needs a dispatch that produced nothing.
	j = neverExecutedTestRunTask(t)
	j, d = neverExecutedDispatch(t, j)
	r := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: refusedBeforeLaunchClass, Detail: "refused", Output: json.RawMessage(`{"exit_code":0}`)}
	if _, _, err := engine.Complete(j, d.ID, r); err == nil {
		t.Fatal("a refusal before launch with output was accepted")
	}
}

// FAE-RT-004: an agent turn refused before its provider launched (the
// bridge's refused-before-provider, a missing brief) charges no attempt, and
// a later review session the bridge refused before its provider ran is
// refunded too: the reviews that ran are named, not recorded, and the next
// dispatch runs every reviewer in fresh sessions (FAE-WF-033).
func TestRefusedBeforeLaunchOfAnAgentTurnOrLaterReviewer(t *testing.T) {
	x := newTurnBoundsTask(t)
	x.run(refusedBeforeLaunchClass, refusedBeforeLaunchClass, refusedBeforeLaunchClass)
	if x.j.Attempts != 0 || x.j.State != "intake" || x.events("failure", 0) != 3 {
		t.Fatalf("agent turn refusals: attempts %d %s failures %d", x.j.Attempts, x.j.State, x.events("failure", 0))
	}
	x.run(refusedBeforeLaunchClass)
	if !x.j.Escalated(domain.SameGate) || x.j.Attempts != 0 {
		t.Fatalf("fourth agent turn refusal: %s %s attempts %d", x.j.State, x.j.EscalationKind, x.j.Attempts)
	}

	j, options := laterReviewerStartTask(t, map[string]bool{"m1": true, "s1": true, "m2": true})
	j, d := laterReviewerStartDispatch(t, j, options)
	ran := []engine.ReviewResult{{Session: d.Sessions[0].ID, Role: "a", Output: json.RawMessage(`{"approved":true}`)}}
	r := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: refusedBeforeLaunchClass, Detail: "bridge refused the turn before the provider ran: task files over 4 MiB", Reviews: ran}
	next, events, err := engine.Complete(j, d.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	next = roundTrip(t, next)
	if next.Attempts != 0 || next.Counts["review"] != 0 || len(next.Evidence) != 0 || next.InFlight != nil {
		t.Fatalf("later reviewer refused before launch: attempts %d visits %d evidence %d", next.Attempts, next.Counts["review"], len(next.Evidence))
	}
	if failure := neverExecutedEvent(events, "failure"); !strings.HasPrefix(failure.Cause, "RefusedBeforeLaunch: bridge refused the turn") || !strings.Contains(failure.Cause, "after 1 of 2 review sessions ran: "+d.Sessions[0].ID+" (a)") || !strings.Contains(failure.Cause, "no attempt is charged") {
		t.Fatalf("events %+v", events)
	}
	_, again := laterReviewerStartDispatch(t, next, options)
	if again.Attempt != 1 || len(again.Sessions) != 2 || again.Sessions[0].ID == d.Sessions[0].ID {
		t.Fatalf("redispatch %+v attempt %d", again.Sessions, again.Attempt)
	}
	for name, mutate := range map[string]func(*engine.Result){
		"foreign review session": func(r *engine.Result) { r.Reviews[0].Session = "other" },
		"every review collected": func(r *engine.Result) {
			r.Reviews = append(r.Reviews, engine.ReviewResult{Session: d.Sessions[1].ID, Role: "b", Output: json.RawMessage(`{"approved":true}`)})
		},
		"a new tree": func(r *engine.Result) { r.NewTree = "changed" },
	} {
		bad := r
		bad.Reviews = append([]engine.ReviewResult{}, r.Reviews...)
		mutate(&bad)
		if _, _, err := engine.Complete(j, d.ID, bad); err == nil {
			t.Fatalf("%s: accepted an invalid refusal before launch", name)
		}
	}
}

// FAE-RT-004 (new in wave 11, re-audit 10 probe H): the agent-start bound is
// per binding. A review dispatch's later reviewer fails to start twice on
// s1, then the first reviewer fails once on m1: m1 has failed once, so
// nothing parks. s1's third failure parks ProviderUnavailable naming s1, and
// m1 keeps its one failure. None of it charges an attempt.
func TestAgentStartBoundIsPerBinding(t *testing.T) {
	j, options := laterReviewerStartTask(t, map[string]bool{"m1": true, "s1": true, "m2": true})
	m1 := engine.AgentStartCountKey("review", workflow.Binding{Provider: "claude", Model: "m1"})
	s1 := engine.AgentStartCountKey("review", workflow.Binding{Provider: "claude", Model: "s1"})
	later := func() {
		t.Helper()
		var d *engine.Dispatch
		j, d = laterReviewerStartDispatch(t, j, options)
		next, _, err := engine.Complete(j, d.ID, laterReviewerStartResult(d))
		if err != nil {
			t.Fatal(err)
		}
		j = roundTrip(t, next)
	}
	later()
	later()
	j, d := laterReviewerStartDispatch(t, j, options)
	r := laterReviewerStartResult(d)
	r.Class, r.Reviews = "startup-retry", nil
	next, events, err := engine.Complete(j, d.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	j = roundTrip(t, next)
	if j.Parked || events[len(events)-1].Kind != "startup-retry" || j.Failures[m1] != 1 || j.Failures[s1] != 2 {
		t.Fatalf("one failed start of m1 after two of s1: parked=%v %q events %+v failures %v", j.Parked, j.Reason, events, j.Failures)
	}
	j, d = laterReviewerStartDispatch(t, j, options)
	next, events, err = engine.Complete(j, d.ID, laterReviewerStartResult(d))
	if err != nil {
		t.Fatal(err)
	}
	j = roundTrip(t, next)
	if !j.Parked || j.Reason != engine.ParkProviderUnavailable || !strings.HasPrefix(events[len(events)-1].Cause, engine.ParkProviderUnavailable+": startup exhausted on claude/s1") || j.Failures[s1] != 0 || j.Failures[m1] != 1 || j.Attempts != 0 {
		t.Fatalf("s1's third failed start: parked=%v %q events %+v failures %v attempts %d", j.Parked, j.Reason, events, j.Failures, j.Attempts)
	}
}

// FAE-HOST-006, FAE-RT-004: a later review session whose host admission
// deferred it before launch parks infrastructure-deferred (the park the
// first session's deferral takes; the daemon reports PaneLimit or the host
// guard) and charges nothing. Its reviews must be the earlier sessions', in
// order, and only a review dispatch carries them.
func TestLaterSessionAdmissionDeferralChargesNothing(t *testing.T) {
	j, options := laterReviewerStartTask(t, map[string]bool{"m1": true, "s1": true, "m2": true})
	j, d := laterReviewerStartDispatch(t, j, options)
	r := laterReviewerStartResult(d)
	r.Class, r.Detail = "infrastructure-deferred", "host admission: PaneLimit"
	for name, mutate := range map[string]func(*engine.Result){
		"foreign review session": func(r *engine.Result) { r.Reviews[0].Session = "other" },
		"output alongside":       func(r *engine.Result) { r.Output = json.RawMessage(`{"approved":true}`) },
		"every review collected": func(r *engine.Result) {
			r.Reviews = append(r.Reviews, engine.ReviewResult{Session: d.Sessions[1].ID, Role: "b", Output: json.RawMessage(`{"approved":true}`)})
		},
	} {
		bad := r
		bad.Reviews = append([]engine.ReviewResult{}, r.Reviews...)
		mutate(&bad)
		if _, _, err := engine.Complete(j, d.ID, bad); err == nil {
			t.Fatalf("%s: accepted an invalid later-session deferral", name)
		}
	}
	next, _, err := engine.Complete(j, d.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	next = roundTrip(t, next)
	if !next.Parked || next.Reason != "infrastructure-deferred" || next.Attempts != 0 || next.Counts["review"] != 0 || len(next.Evidence) != 0 || next.InFlight != nil {
		t.Fatalf("later-session deferral: parked=%v %q attempts %d visits %d evidence %d", next.Parked, next.Reason, next.Attempts, next.Counts["review"], len(next.Evidence))
	}
}

// Every path that no longer charges an attempt still ends (FAE-RT-004's
// start bound, FAE-RT-010's route-exhaustion clock, SameGate and the attempt
// limit where a provider ran): a reviewer that never starts on any route, a
// reviewer whose every route the bridge refuses at every daemon start, and
// refusals before launch between charged failures whose reasons vary.
func TestNeverExecutedPathsStillTerminate(t *testing.T) {
	t.Run("later reviewer never starts on any route", func(t *testing.T) {
		healthy := map[string]bool{"m1": true, "s1": true, "m2": true}
		j, options := laterReviewerStartTask(t, healthy)
		runs := 0
		for step := 0; ; step++ {
			if step > 20 {
				t.Fatalf("no end after %d steps: %s parked=%v %q", step, j.State, j.Parked, j.Reason)
			}
			next, d, _, err := engine.PrepareWith(j, options)
			if err != nil {
				t.Fatal(err)
			}
			j = roundTrip(t, next)
			if j.State == domain.Escalated {
				break
			}
			if d == nil && !j.Parked {
				continue
			}
			if d == nil {
				if !engine.RoutesUnavailableReason(j.Reason) {
					t.Fatalf("step %d: parked %q", step, j.Reason)
				}
				// Past the route-exhaustion bound the park escalates.
				j.RoutesUnavailableSince = time.Now().UTC().Add(-engine.DefaultRouteExhaustion - time.Minute)
				continue
			}
			runs++
			next, _, err = engine.Complete(j, d.ID, laterReviewerStartResult(d))
			if err != nil {
				t.Fatal(err)
			}
			j = roundTrip(t, next)
			if j.Parked && j.Reason == engine.ParkProviderUnavailable {
				// The daemon charges the exhausted binding Unavailable.
				healthy[d.Sessions[1].Route.Model] = false
			}
		}
		if !j.Escalated(domain.ProviderUnavailable) || j.Attempts != 0 || runs != 2*engine.AgentStartAttempts {
			t.Fatalf("end: %s %s attempts %d reviewer-a runs %d", j.State, j.EscalationKind, j.Attempts, runs)
		}
	})
	t.Run("later reviewer refused on every route at every daemon start", func(t *testing.T) {
		j, options := laterReviewerTask(t, map[string]bool{"m1": true, "c1": true, "m2": true})
		refuseB := func() {
			t.Helper()
			for range 2 {
				var d *engine.Dispatch
				j, d = laterReviewerDispatch(t, j, options)
				route := d.Sessions[1].Route
				r := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: engine.ClassBindingRefused, Detail: "no credentials", RefusedRoute: &route, Reviews: []engine.ReviewResult{{Session: d.Sessions[0].ID, Role: "a", Output: json.RawMessage(`{"approved":true}`)}}}
				next, _, err := engine.Complete(j, d.ID, r)
				if err != nil {
					t.Fatal(err)
				}
				j = roundTrip(t, next)
			}
		}
		park := func() {
			t.Helper()
			next, d, _, err := engine.PrepareWith(j, options)
			if err != nil || d != nil || !next.Parked || !engine.RoutesUnavailableReason(next.Reason) {
				t.Fatalf("every route of b refused: %v dispatch %v parked=%v %q", err, d != nil, next.Parked, next.Reason)
			}
			j = roundTrip(t, next)
		}
		refuseB()
		park()
		// 25 minutes on, the daemon restarts and the refusals recur.
		since := time.Now().UTC().Add(-25 * time.Minute).Truncate(time.Second)
		j.RoutesUnavailableSince = since
		options.RouteGeneration = "daemon-2"
		refuseB()
		park()
		if !j.RoutesUnavailableSince.Equal(since) || j.Attempts != 0 {
			t.Fatalf("a restart whose refusals recur restarted the clock or charged: since %v (want %v) attempts %d", j.RoutesUnavailableSince, since, j.Attempts)
		}
		// Another restart 10 minutes later: past the bound, it escalates.
		j.RoutesUnavailableSince = since.Add(-10 * time.Minute)
		options.RouteGeneration = "daemon-3"
		refuseB()
		next, d, _, err := engine.PrepareWith(j, options)
		if err != nil || d != nil || !next.Escalated(domain.ProviderUnavailable) || next.Attempts != 0 {
			t.Fatalf("a refusal recurring at every start never escalated: %v dispatch %v %s %q attempts %d", err, d != nil, next.State, next.Reason, next.Attempts)
		}
	})
	t.Run("later session deferred by the host stays parked beyond the historical bound", func(t *testing.T) {
		j, options := laterReviewerStartTask(t, map[string]bool{"m1": true, "s1": true, "m2": true})
		for n := 1; n <= engine.AdmissionDeferrals+40; n++ {
			j.AdmissionRetryAfter = time.Time{}
			var d *engine.Dispatch
			j, d = laterReviewerStartDispatch(t, j, options)
			r := laterReviewerStartResult(d)
			r.Class, r.Detail = "infrastructure-deferred", "host admission: MemoryReservation"
			next, events, err := engine.Complete(j, d.ID, r)
			if err != nil {
				t.Fatal(err)
			}
			j = roundTrip(t, next)
			if !j.Parked || j.Reason != "infrastructure-deferred" || j.State == domain.Escalated || j.Attempts != 0 || j.Counts["review"] != 0 || len(j.Evidence) != 0 || j.Failures["admission-deferred:review"] != n || neverExecutedEvent(events, "park").Cause != "infrastructure-deferred" {
				t.Fatalf("deferral %d: state=%s parked=%v reason=%q attempts=%d failures=%v", n, j.State, j.Parked, j.Reason, j.Attempts, j.Failures)
			}
		}
		// Once capacity returns, both fresh reviewers complete and the task advances.
		j.AdmissionRetryAfter = time.Time{}
		j, d := laterReviewerStartDispatch(t, j, options)
		ok := json.RawMessage(`{"approved":true}`)
		next, _, err := engine.Complete(j, d.ID, engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Reviews: []engine.ReviewResult{{Session: d.Sessions[0].ID, Role: "a", Output: ok}, {Session: d.Sessions[1].ID, Role: "b", Output: ok}}})
		if err != nil || next.State == "review" || next.Failures["admission-deferred:review"] != 0 || len(next.Evidence) != 2 {
			t.Fatalf("capacity restored: %v state=%s failures=%v", err, next.State, next.Failures)
		}
	})
	t.Run("effect and first-session deferrals are not counted, and a completed turn clears later-session counts", func(t *testing.T) {
		// A test run deferred by the heavy lease or the integration queue
		// launches no provider: it stays a park re-decided each round.
		j := neverExecutedTestRunTask(t)
		for n := 1; n <= engine.AdmissionDeferrals+2; n++ {
			j.AdmissionRetryAfter = time.Time{}
			if j.Parked && j.Reason == "infrastructure-deferred" {
				j.Parked, j.Reason = false, ""
			}
			var d *engine.Dispatch
			j, d = neverExecutedDispatch(t, j)
			j, _ = neverExecutedComplete(t, j, d, "infrastructure-deferred", "integration queue: task other holds the ref", nil)
			if !j.Parked || j.Reason != "infrastructure-deferred" || j.Attempts != 0 || j.TestRuns != 0 || len(j.Failures) != 0 {
				t.Fatalf("effect deferral %d: parked=%v %q attempts %d test runs %d failures %v", n, j.Parked, j.Reason, j.Attempts, j.TestRuns, j.Failures)
			}
		}
		// FAE-HOST-006, FAE-VER-003 (re-audit 12 ra12I/m/deferfirst): a
		// first-session deferral launches nothing and spends no provider
		// turn, so it stays a traced park re-decided each round without
		// bound and never fails the task.
		x := newTurnBoundsTask(t)
		for n := 1; n <= 200; n++ {
			x.j.Parked, x.j.Reason, x.j.AdmissionRetryAfter = false, "", time.Time{}
			x.run("infrastructure-deferred")
			if !x.j.Parked || x.j.Reason != "infrastructure-deferred" || x.j.State == domain.Escalated || x.j.Attempts != 0 || len(x.j.Failures) != 0 {
				t.Fatalf("first-session deferral %d: state=%s parked=%v %q attempts=%d failures=%v", n, x.j.State, x.j.Parked, x.j.Reason, x.j.Attempts, x.j.Failures)
			}
		}
		// Later-session review deferrals (where earlier reviewers already
		// ran) carry across a start failure of the same turn and clear once
		// the turn completes.
		rj, options := laterReviewerStartTask(t, map[string]bool{"m1": true, "s1": true, "m2": true})
		for range 2 {
			rj.AdmissionRetryAfter = time.Time{}
			var d *engine.Dispatch
			rj, d = laterReviewerStartDispatch(t, rj, options)
			r := laterReviewerStartResult(d)
			r.Class, r.Detail = "infrastructure-deferred", "host admission: MemoryReservation"
			next, _, err := engine.Complete(rj, d.ID, r)
			if err != nil {
				t.Fatal(err)
			}
			rj = roundTrip(t, next)
		}
		rj.AdmissionRetryAfter = time.Time{}
		rj, d := laterReviewerStartDispatch(t, rj, options)
		next, _, err := engine.Complete(rj, d.ID, laterReviewerStartResult(d))
		if err != nil {
			t.Fatal(err)
		}
		rj = roundTrip(t, next)
		if rj.Failures["admission-deferred:review"] != 2 {
			t.Fatalf("a start failure of the same turn cleared the later-session deferrals: %v", rj.Failures)
		}
		rj, d = laterReviewerStartDispatch(t, rj, options)
		ok := json.RawMessage(`{"approved":true}`)
		next, _, err = engine.Complete(rj, d.ID, engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Reviews: []engine.ReviewResult{{Session: d.Sessions[0].ID, Role: "a", Output: ok}, {Session: d.Sessions[1].ID, Role: "b", Output: ok}}})
		if err != nil {
			t.Fatal(err)
		}
		rj = roundTrip(t, next)
		if rj.Failures["admission-deferred:review"] != 0 || rj.State == "review" {
			t.Fatalf("a completed review turn kept its deferrals: %s %v", rj.State, rj.Failures)
		}
	})
	t.Run("later reviewer never starts while another task heals the binding", func(t *testing.T) {
		// Re-audit 11/12 (probe ra11D P5, ra12D P7, ra12B E1b): s1's third
		// failed start parks ProviderUnavailable, another task's ok heals s1
		// at once, the park is re-decided and s1 gets three fresh starts,
		// without end. The turn's starts over every binding (3 per declared
		// binding: m1, s1 and m2) are counted across the exhaustions; once
		// that bound (9) is reached, no 10th start is launched and the turn
		// escalates ProviderUnavailable naming them.
		healthy := map[string]bool{"m1": true, "s1": true, "m2": true}
		j, options := laterReviewerStartTask(t, healthy)
		bound := engine.StageAgentStartBound(j, "review")
		runs, exhaustions := 0, 0
		for step := 0; ; step++ {
			if step > 40 {
				t.Fatalf("no end after %d steps: %s parked=%v %q", step, j.State, j.Parked, j.Reason)
			}
			next, d, _, err := engine.PrepareWith(j, options)
			if err != nil {
				t.Fatal(err)
			}
			j = roundTrip(t, next)
			if j.State == domain.Escalated {
				break
			}
			if d == nil {
				continue
			}
			runs++
			next, _, err = engine.Complete(j, d.ID, laterReviewerStartResult(d))
			if err != nil {
				t.Fatal(err)
			}
			j = roundTrip(t, next)
			if j.Parked && j.Reason == engine.ParkProviderUnavailable {
				exhaustions++
				if j.Failures["agent-starts:review"] != runs {
					t.Fatalf("exhaustion %d re-armed the turn's total: %v", exhaustions, j.Failures)
				}
			}
		}
		if bound != 3*engine.AgentStartAttempts || !j.Escalated(domain.ProviderUnavailable) || j.Attempts != 0 || runs != bound || exhaustions != 3 || !strings.Contains(j.EscalationDetail, "review: 9 agent starts failed in one turn, reaching the turn's bound of 9 (3 per binding over claude/m1, claude/s1, claude/m2); the last, on claude/s1:") || !strings.Contains(j.EscalationDetail, "after 1 of 2 review sessions ran") {
			t.Fatalf("end: %s %s %q attempts %d reviewer-a runs %d exhaustions %d bound %d", j.State, j.EscalationKind, j.EscalationDetail, j.Attempts, runs, exhaustions, bound)
		}
	})
	t.Run("first session never starts while another task heals the binding", func(t *testing.T) {
		// Re-audit 11/12 (probe ra11D P1, ra12B E1c): one binding, exhausted
		// and healed without end. Its three starts are the turn's bound; no
		// fourth start is launched.
		x := newTurnBoundsTask(t)
		single := x.j.Document
		for name, role := range single.Roles {
			role.Routes = role.Routes[:1]
			single.Roles[name] = role
		}
		j, err := engine.NewLive("healed", 1, single, "tree")
		if err != nil {
			t.Fatal(err)
		}
		x.j = apply(t, j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: 100})
		starts := 0
		for step := 0; step < 20 && x.j.State != domain.Escalated; step++ {
			next, d, _, err := engine.PrepareWith(x.j, x.options)
			if err != nil {
				t.Fatal(err)
			}
			x.j = roundTrip(t, next)
			if d == nil {
				continue
			}
			starts++
			x.complete(d, "startup-retry")
		}
		if !x.j.Escalated(domain.ProviderUnavailable) || x.j.Attempts != 0 || starts != engine.AgentStartAttempts || !strings.Contains(x.j.EscalationDetail, "intake: 3 agent starts failed in one turn, reaching the turn's bound of 3 (3 per binding over p/one)") {
			t.Fatalf("end: %s %s %q attempts %d starts %d", x.j.State, x.j.EscalationKind, x.j.EscalationDetail, x.j.Attempts, starts)
		}
		// One binding with a failover after two failed starts: the third
		// failed start reaches the turn's total of 3 and escalates on the
		// third start (probe E1c).
		z, err := engine.NewLive("healed-failover", 1, single, "tree")
		if err != nil {
			t.Fatal(err)
		}
		x.j = apply(t, z, engine.Control{Method: "SetAttemptLimit", AttemptLimit: 100})
		x.run("startup-retry", "startup-retry", engine.ClassProviderFailover)
		x.healthy["one"] = true
		x.run("startup-retry")
		if !x.j.Escalated(domain.ProviderUnavailable) || x.j.Attempts != 0 || !strings.Contains(x.j.EscalationDetail, "intake: 3 agent starts failed in one turn, reaching the turn's bound of 3 (3 per binding over p/one)") {
			t.Fatalf("failover + third start: %s %s %q attempts %d", x.j.State, x.j.EscalationKind, x.j.EscalationDetail, x.j.Attempts)
		}
		// Four bindings: a failover gives the next route fresh per-binding
		// starts (TestTurnBoundsRestartAfterAFailedTurn), but the total
		// carries, and a provider that ran clears it.
		y := newTurnBoundsTask(t)
		y.run("startup-retry", "startup-retry", engine.ClassProviderFailover, "startup-retry")
		if y.j.Failures["agent-starts:intake"] != 3 || agentStarts(y.j, "intake") != 1 {
			t.Fatalf("after a failover: failures %v", y.j.Failures)
		}
		y.run("success")
		if y.j.Failures["agent-starts:intake"] != 0 {
			t.Fatalf("a completed turn kept its total: %v", y.j.Failures)
		}
	})
	t.Run("later reviewer rejected-change carrying earlier reviews escalates StaleBranchResidue", func(t *testing.T) {
		j, options := laterReviewerStartTask(t, map[string]bool{"m1": true, "s1": true, "m2": true})
		j, d := laterReviewerStartDispatch(t, j, options)
		r := laterReviewerStartResult(d)
		r.Class, r.Detail = engine.ClassRejectedChange, "read-only role modified task tree"
		next, _, err := engine.Complete(j, d.ID, r)
		if err != nil {
			t.Fatal(err)
		}
		next = roundTrip(t, next)
		if !next.Escalated(domain.StaleBranchResidue) || len(next.Evidence) != 0 || len(next.Provenance) != 0 || !strings.Contains(next.EscalationDetail, "read-only role modified task tree") {
			t.Fatalf("later reviewer rejected-change: %s %s %q evidence=%d", next.State, next.EscalationKind, next.EscalationDetail, len(next.Evidence))
		}
	})
	t.Run("refund after restart at unvisited start stage restores NewAttempt", func(t *testing.T) {
		x := newTurnBoundsTask(t)
		x.j.Attempts, x.j.NewAttempt = 3, true
		x.j = roundTrip(t, x.j)
		d := x.dispatch()
		if !d.AttemptCharged || x.j.Attempts != 4 {
			t.Fatalf("first dispatch after NewAttempt: charged=%v attempts=%d", d.AttemptCharged, x.j.Attempts)
		}
		x.complete(d, refusedBeforeLaunchClass)
		if x.j.Attempts != 3 || !x.j.NewAttempt || x.j.Counts["intake"] != 0 {
			t.Fatalf("after refund: attempts=%d newAttempt=%v visits=%d", x.j.Attempts, x.j.NewAttempt, x.j.Counts["intake"])
		}
		d2 := x.dispatch()
		if !d2.AttemptCharged || x.j.Attempts != 4 {
			t.Fatalf("redispatch after refund: charged=%v attempts=%d", d2.AttemptCharged, x.j.Attempts)
		}
	})
	t.Run("unlaunched crash replay and non-execution uncertainty resolution refund charges", func(t *testing.T) {
		x := newTurnBoundsTask(t)
		for n := 1; n <= 10; n++ {
			d := x.dispatch()
			replayed, _, err := engine.ReplayUnlaunched(x.j, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			x.j = roundTrip(t, replayed)
			if x.j.InFlight != nil || x.j.Parked || x.j.Attempts != 0 || x.j.Counts["intake"] != 0 || len(x.j.Failures) != 0 {
				t.Fatalf("replay %d: attempts=%d visits=%d failures=%v", n, x.j.Attempts, x.j.Counts["intake"], x.j.Failures)
			}
		}
		j := neverExecutedTestRunTask(t)
		j, d := neverExecutedDispatch(t, j)
		recovered, _, err := engine.Recover(j, false, false)
		if err != nil {
			t.Fatal(err)
		}
		resolved, _, err := engine.ResolveUncertainNonExecution(recovered, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		resolved = roundTrip(t, resolved)
		if resolved.InFlight != nil || resolved.Parked || resolved.Attempts != 0 || resolved.TestRuns != 0 || resolved.Counts["run"] != 0 {
			t.Fatalf("ResolveUncertainNonExecution: attempts=%d testRuns=%d visits=%d", resolved.Attempts, resolved.TestRuns, resolved.Counts["run"])
		}
	})
	t.Run("refusals before launch between charged failures", func(t *testing.T) {
		x := newTurnBoundsTask(t)
		x.control(engine.Control{Method: "SetAttemptLimit", AttemptLimit: 5})
		charged := 0
		for step := 0; ; step++ {
			if step > 40 {
				t.Fatalf("no end after %d steps: attempts %d", step, x.j.Attempts)
			}
			d := x.dispatchOrStop()
			if d == nil {
				break
			}
			if step%4 != 3 {
				x.complete(d, refusedBeforeLaunchClass)
				continue
			}
			// A provider ran and answered badly, each time differently.
			charged++
			next, _, err := engine.Complete(x.j, d.ID, engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: engine.ClassMalformedOutput, Detail: fmt.Sprintf("answer %d malformed", step)})
			if err != nil {
				t.Fatal(err)
			}
			x.j = roundTrip(t, next)
		}
		if !x.j.Escalated(domain.AttemptCap) || charged != 5 || x.j.Attempts != 5 {
			t.Fatalf("end: %s %s %q attempts %d charged %d", x.j.State, x.j.EscalationKind, x.j.EscalationDetail, x.j.Attempts, charged)
		}
	})
}

// dispatchOrStop prepares the task's next dispatch, or returns nil once the
// task escalated or ended.
func (x *turnBoundsTask) dispatchOrStop() *engine.Dispatch {
	x.t.Helper()
	for range 30 {
		j, d, _, err := engine.PrepareWith(x.j, x.options)
		if err != nil {
			x.t.Fatal(err)
		}
		x.j = j
		if d != nil {
			return d
		}
		if j.State == domain.Escalated || j.State.Terminal() {
			return nil
		}
		if j.Parked {
			x.t.Fatalf("parked: %q", j.Reason)
		}
	}
	x.t.Fatal("no dispatch")
	return nil
}

// commitRefusalTask is a live task at its commit stage: a change turn, a
// passing test run and gate, and a review precede the protected commit.
func commitRefusalTask(t *testing.T) (engine.Job, *engine.Dispatch) {
	t.Helper()
	doc := staleWorkflow(false)
	j, err := engine.NewLive("commit-refused", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	ok := json.RawMessage(`{"exit_code":0,"executed_count":1,"fresh_build":true,"approved":true}`)
	for range 40 {
		next, d, _, err := engine.PrepareWith(j, engine.Options{})
		if err != nil {
			t.Fatal(err)
		}
		j = roundTrip(t, next)
		if d == nil {
			if j.Parked || j.State == domain.Escalated || j.State.Terminal() {
				t.Fatalf("no commit dispatch: %s parked=%v %q", j.State, j.Parked, j.Reason)
			}
			continue
		}
		if d.Config.Primitive == workflow.VcsCommit {
			return j, d
		}
		r := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Output: ok}
		if d.Config.Primitive == workflow.Review {
			r.Output, r.Reviews = nil, []engine.ReviewResult{{Session: d.Sessions[0].ID, Role: "reviewer", Output: ok}}
		}
		if j, _, err = engine.Complete(j, d.ID, r); err != nil {
			t.Fatal(d.Stage, err)
		}
		j = roundTrip(t, j)
	}
	t.Fatal("commit stage not reached")
	return j, nil
}

// FAE-RT-004, FAE-WF-074 (operator ruling 2026-10-29): a commit refused
// before any object or ref was written is a deterministic policy refusal
// that ran nothing: the no-red classifier, the missing proof of the always-
// required checks, and a reviewed snapshot or prepared commit the broker
// could not produce. Each still escalates for the operator, but refunds the
// attempt and the visit. A commit that failed after its write was attempted
// (vcs-refused) and an integration refused after its retry stay charged.
func TestCommitRefusedBeforeAnyWriteIsRefunded(t *testing.T) {
	for _, tc := range []struct {
		class    string
		kind     domain.EscalationKind
		refunded bool
		detail   string
	}{
		{engine.ClassNoRedViolation, domain.NoRedViolation, true, "production code without RED: answer.go"},
		{engine.ClassRequiredCheckFailed, domain.HumanDecisionRequired, true, "RequiredCheckFailed: always-required tests and compile check not proven on the committed tree"},
		{"commit-refused-before-write", domain.HumanDecisionRequired, true, "CommitRefused: reviewed tree mismatch"},
		{engine.ClassVcsRefused, domain.HumanDecisionRequired, false, "CommitRefused: task commit failed after one retry: lock held"},
	} {
		t.Run(tc.class, func(t *testing.T) {
			j, d := commitRefusalTask(t)
			attempts, visits := j.Attempts, j.Counts["commit"]
			if visits != 1 {
				t.Fatalf("commit dispatch: visits %d", visits)
			}
			detail := strings.TrimPrefix(strings.TrimPrefix(tc.detail, "RequiredCheckFailed: "), "CommitRefused: ")
			next, _, err := engine.Complete(j, d.ID, engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: tc.class, Detail: detail})
			if err != nil {
				t.Fatal(err)
			}
			next = roundTrip(t, next)
			wantAttempts, wantVisits := attempts, visits
			if tc.refunded {
				// The commit is the attempt's first visit: it charged the
				// visit, and the attempt only if it opened one.
				wantVisits = 0
				if d.AttemptCharged {
					wantAttempts = attempts - 1
				}
			}
			if !next.Escalated(tc.kind) || next.EscalationDetail != tc.detail || next.Attempts != wantAttempts || next.Counts["commit"] != wantVisits || next.InFlight != nil {
				t.Fatalf("%s: %s %s %q attempts %d (want %d) visits %d (want %d)", tc.class, next.State, next.EscalationKind, next.EscalationDetail, next.Attempts, wantAttempts, next.Counts["commit"], wantVisits)
			}
		})
	}
	// The refunded refusal needs a commit dispatch that wrote nothing.
	j, d := commitRefusalTask(t)
	for name, r := range map[string]engine.Result{
		"with output":       {Class: "commit-refused-before-write", Detail: "x", Output: json.RawMessage(`{"sha":"abc"}`)},
		"with a new tree":   {Class: "commit-refused-before-write", Detail: "x", NewTree: "other"},
		"without a detail":  {Class: "commit-refused-before-write"},
		"of an integration": {Class: "commit-refused-before-write", Detail: "x", DispatchID: "other"},
	} {
		r.ExecutionKind, r.TaskID, r.Tree, r.Session, r.ProducedAt = "live", d.TaskID, d.Tree, d.Sessions[0].ID, time.Now().UTC()
		if r.DispatchID == "" {
			r.DispatchID = d.ID
		}
		if _, _, err := engine.Complete(j, d.ID, r); err == nil {
			t.Fatalf("%s: accepted an invalid commit refusal", name)
		}
	}
}

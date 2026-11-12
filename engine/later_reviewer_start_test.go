package engine_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// laterReviewerStartClass is the executor class of a review dispatch whose
// later session never started (engine.ClassLaterReviewerNotStarted), spelled
// out so the test pins its wire value.
const laterReviewerStartClass = "later-reviewer-not-started"

// laterReviewerStartTask is a two-reviewer review stage: reviewer a on m1,
// reviewer b on s1 (a binding that never starts) and then m2. healthy
// decides which bindings route selection may take.
func laterReviewerStartTask(t *testing.T, healthy map[string]bool) (engine.Job, engine.Options) {
	t.Helper()
	approved := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "review", Path: []string{"approved"}}, {Op: "literal", Value: true}}}
	doc := workflow.Document{SchemaVersion: 1, ID: "later-reviewer-start", Start: "review", Limits: workflow.Limits{Attempts: 8, TestRuns: 2, ScopeWidens: 1},
		Roles: map[string]workflow.Role{
			"a": {Instructions: "review", Briefs: map[string]string{"default": "brief"}, Routes: []workflow.Binding{{Provider: "claude", Model: "m1"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"Verdict"}},
			"b": {Instructions: "review", Briefs: map[string]string{"default": "brief"}, Routes: []workflow.Binding{{Provider: "claude", Model: "s1"}, {Provider: "claude", Model: "m2"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"Verdict"}},
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
	j, err := engine.NewLive("later-reviewer-start", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	return j, engine.Options{RouteGeneration: "daemon-1", SelectRoute: func(routes []workflow.Binding) (workflow.Binding, bool) {
		for _, r := range routes {
			if healthy[r.Model] {
				return r, true
			}
		}
		return workflow.Binding{}, false
	}}
}

// laterReviewerStartDispatch prepares the task's next review dispatch.
func laterReviewerStartDispatch(t *testing.T, j engine.Job, options engine.Options) (engine.Job, *engine.Dispatch) {
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
	t.Fatalf("no dispatch: parked=%v reason=%q state=%s", j.Parked, j.Reason, j.State)
	return j, nil
}

// laterReviewerStartResult is the executor's result for a dispatch whose
// first review ran and whose second session never started.
func laterReviewerStartResult(d *engine.Dispatch) engine.Result {
	return engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: laterReviewerStartClass, Detail: "the provider never acknowledged the prompt: provider exited before acknowledging the prompt",
		Reviews: []engine.ReviewResult{{Session: d.Sessions[0].ID, Role: "a", Output: json.RawMessage(`{"approved":true}`)}}}
}

// FAE-RT-004 (operator ruling 2026-10-29: a run never executed doesn't count
// towards the budget), FAE-WF-033: a review dispatch whose second session
// provably never started, after the first reviewer ran, is a typed start
// failure, not an UncertainDispatch park. The session that failed never ran,
// so the dispatch charges no attempt or visit; no partial review is recorded,
// the start counts toward the agent-start bound of that session's binding,
// and the stage runs both reviewers again in fresh sessions. The binding's
// third failed start parks ProviderUnavailable naming it (the daemon charges
// it), and route selection then moves that reviewer to its next route, where
// the dispatch is still the task's first attempt.
func TestLaterReviewerStartFailureRerunsEveryReviewerWithinTheStartBound(t *testing.T) {
	healthy := map[string]bool{"m1": true, "s1": true, "m2": true}
	j, options := laterReviewerStartTask(t, healthy)
	s1 := engine.AgentStartCountKey("review", workflow.Binding{Provider: "claude", Model: "s1"})
	for n := 1; n <= 3; n++ {
		var d *engine.Dispatch
		j, d = laterReviewerStartDispatch(t, j, options)
		if d.Attempt != 1 || d.Visit != 1 || len(d.Sessions) != 2 || d.Sessions[0].Route.Model != "m1" || d.Sessions[1].Route.Model != "s1" {
			t.Fatalf("start %d: dispatch %+v attempt %d visit %d, want attempt and visit 1", n, d.Sessions, d.Attempt, d.Visit)
		}
		next, events, err := engine.Complete(j, d.ID, laterReviewerStartResult(d))
		if err != nil {
			t.Fatalf("start %d: %v", n, err)
		}
		j = roundTrip(t, next)
		if j.Parked && j.Reason == "UncertainDispatch" {
			t.Fatalf("start %d: a later session's start failure parked UncertainDispatch", n)
		}
		if j.Attempts != 0 || j.Counts["review"] != 0 || j.InFlight != nil || len(j.Evidence) != 0 {
			t.Fatalf("start %d: attempts %d visits %d evidence %d: a session that never ran must charge nothing and record nothing", n, j.Attempts, j.Counts["review"], len(j.Evidence))
		}
		last := events[len(events)-1]
		if !strings.Contains(last.Cause, "after 1 of 2 review sessions ran: "+d.Sessions[0].ID+" (a)") || !strings.Contains(last.Cause, "no attempt is charged") {
			t.Fatalf("start %d: event does not name the review that ran: %+v", n, last)
		}
		if n < 3 {
			if j.Parked || last.Kind != "startup-retry" || j.Failures[s1] != n {
				t.Fatalf("start %d: parked=%v event %+v failures %v", n, j.Parked, last, j.Failures)
			}
			continue
		}
		if !j.Parked || j.Reason != engine.ParkProviderUnavailable || last.Kind != "park" || !strings.HasPrefix(last.Cause, engine.ParkProviderUnavailable+": startup exhausted on claude/s1") || j.Failures[s1] != 0 {
			t.Fatalf("exhaustion: parked=%v reason %q event %+v failures %v", j.Parked, j.Reason, last, j.Failures)
		}
	}
	// The daemon charged s1 Unavailable: the next dispatch runs both
	// reviewers, b on its next route, and the reviews are recorded.
	healthy["s1"] = false
	j, d := laterReviewerStartDispatch(t, j, options)
	if d.Attempt != 1 || d.Sessions[0].Route.Model != "m1" || d.Sessions[1].Route.Model != "m2" {
		t.Fatalf("after exhaustion: %+v attempt %d", d.Sessions, d.Attempt)
	}
	reviews := []engine.ReviewResult{{Session: d.Sessions[0].ID, Role: "a", Output: json.RawMessage(`{"approved":true}`)}, {Session: d.Sessions[1].ID, Role: "b", Output: json.RawMessage(`{"approved":true}`)}}
	done, _, err := engine.Complete(j, d.ID, engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Reviews: reviews})
	if err != nil || len(done.Evidence) != 2 || done.Provenance[1].Model != "m2" {
		t.Fatalf("second route: %v evidence %d", err, len(done.Evidence))
	}
}

// The class needs reviews that ran, in order, short of the session that
// failed; a first session's start failure stays the refunded startup-retry,
// which never carries reviews.
func TestLaterReviewerStartFailureEnvelope(t *testing.T) {
	healthy := map[string]bool{"m1": true, "s1": true, "m2": true}
	j, options := laterReviewerStartTask(t, healthy)
	j, d := laterReviewerStartDispatch(t, j, options)
	for name, mutate := range map[string]func(*engine.Result){
		"no review ran":          func(r *engine.Result) { r.Reviews = nil },
		"foreign review session": func(r *engine.Result) { r.Reviews[0].Session = "other" },
		"every review collected": func(r *engine.Result) {
			r.Reviews = append(r.Reviews, engine.ReviewResult{Session: d.Sessions[1].ID, Role: "b", Output: json.RawMessage(`{"approved":true}`)})
		},
		"output alongside":        func(r *engine.Result) { r.Output = json.RawMessage(`{"approved":true}`) },
		"startup-retry with runs": func(r *engine.Result) { r.Class = "startup-retry" },
	} {
		r := laterReviewerStartResult(d)
		mutate(&r)
		if _, _, err := engine.Complete(j, d.ID, r); err == nil {
			t.Fatalf("%s: accepted an invalid later-session start failure", name)
		}
	}
	// A first session's start failure is still refunded.
	r := laterReviewerStartResult(d)
	r.Class, r.Reviews = "startup-retry", nil
	next, _, err := engine.Complete(j, d.ID, r)
	if err != nil || next.Attempts != 0 || next.Counts["review"] != 0 || next.Failures[engine.AgentStartCountKey("review", d.Sessions[0].Route)] != 1 {
		t.Fatalf("first-session start failure: %v attempts %d visits %d failures %v", err, next.Attempts, next.Counts["review"], next.Failures)
	}
}

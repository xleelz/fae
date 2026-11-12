package engine_test

import (
	"context"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// assertKind checks the closed kind, the operator reason and the typed
// transition record that history and the event stream expose.
func assertKind(t *testing.T, j engine.Job, kind domain.EscalationKind, detail string) {
	t.Helper()
	if j.State != domain.Escalated || j.EscalationKind != kind || !kind.Valid() || !strings.Contains(j.EscalationDetail, detail) || !strings.HasPrefix(j.Reason, string(kind)) {
		t.Fatalf("want %s (%s), got %s %s %q %q", kind, detail, j.State, j.EscalationKind, j.EscalationDetail, j.Reason)
	}
	found := false
	for _, e := range j.History {
		if e.To == domain.Escalated || e.Kind == "escalation" {
			found = e.EscalationKind == kind && e.Attempt >= 1 && e.Cause == j.Reason
		}
	}
	if !found || engine.Validate(j) != nil {
		t.Fatal("escalation transition is untyped", j.History[len(j.History)-1])
	}
	j = roundTrip(t, j)
	if j.EscalationKind != kind {
		t.Fatal("kind not durable")
	}
}
func padded(n int) map[string][]engine.ScriptStep {
	script := defaults.HappyScript()
	for k, steps := range script {
		for len(steps) < n {
			steps = append(steps, steps[0])
		}
		script[k] = steps
	}
	return script
}
func liveDrive(t *testing.T, j engine.Job, executor engine.Executor, adjust func(*engine.Dispatch, *engine.Result)) engine.Job {
	t.Helper()
	for i := 0; i < 600 && !j.State.Terminal() && j.State != domain.Escalated && !j.Parked; i++ {
		var d *engine.Dispatch
		var e error
		j, d, _, e = engine.Prepare(j)
		if e != nil {
			t.Fatal(e)
		}
		if d == nil {
			continue
		}
		r, e := executor.Execute(context.Background(), *d)
		if e != nil {
			t.Fatal(e)
		}
		r.ExecutionKind = "live"
		adjust(d, &r)
		if j, _, e = engine.Complete(j, d.ID, r); e != nil {
			t.Fatal(e)
		}
		j = roundTrip(t, j)
	}
	return j
}

func TestEscalationKindsAreTypedAtEverySite(t *testing.T) {
	t.Run("workflow stop defaults to HumanDecisionRequired", func(t *testing.T) {
		script := defaults.HappyScript()
		script["intake"][0].Output = json.RawMessage(`{"accepted":false}`)
		assertKind(t, drive(t, newJob(t), engine.NewScripted(script), true), domain.HumanDecisionRequired, "intake did not accept task")
	})
	t.Run("declared Escalate kind", func(t *testing.T) {
		doc := defaults.Workflow()
		stop := doc.Stages["stop"]
		stop.Kind = domain.IntegrationConflict
		doc.Stages["stop"] = stop
		j, e := engine.New("declared", 1, doc, "tree")
		if e != nil {
			t.Fatal(e)
		}
		script := defaults.HappyScript()
		script["intake"][0].Output = json.RawMessage(`{"accepted":false}`)
		assertKind(t, drive(t, j, engine.NewScripted(script), true), domain.IntegrationConflict, "intake did not accept task")
	})
	t.Run("LoopCap names the loop", func(t *testing.T) {
		script := padded(10)
		rejected := json.RawMessage(`{"approved":false,"findings":[{"finding":"ambiguous","severity":"blocking"}]}`)
		for i := range script["spec-review"] {
			script["spec-review"][i].Reviews = []json.RawMessage{rejected, rejected}
		}
		assertKind(t, drive(t, newJob(t), engine.NewScripted(script), true), domain.LoopCap, "spec-loop exhausted 3 iterations")
	})
	t.Run("SameGate after repeated identical failures", func(t *testing.T) {
		script := padded(10)
		for i := range script["run-red"] {
			script["run-red"][i].Output = json.RawMessage(`{"exit_code":1,"executed_count":1,"matched_count":1,"expected_failure":false,"test_in_source":true,"red_passed":false,"fresh_build":true,"output_digest":"same"}`)
		}
		j := drive(t, newJob(t), engine.NewScripted(script), true)
		assertKind(t, j, domain.SameGate, "red-gate failed 4 times for the same reason")
		if j.Counts["red-loop"] != 4 || j.TestRuns != 4 {
			t.Fatal("same-gate bound not applied before the loop and test-run caps", j.Counts, j.TestRuns)
		}
	})
	t.Run("SameGate restarts on a new reason", func(t *testing.T) {
		script := padded(10)
		for i := range script["run-red"] {
			script["run-red"][i].Output = json.RawMessage(`{"exit_code":1,"executed_count":1,"matched_count":1,"expected_failure":false,"test_in_source":true,"red_passed":false,"fresh_build":true,"output_digest":"a"}`)
			if i%2 == 1 {
				script["run-red"][i].Output = json.RawMessage(`{"exit_code":1,"executed_count":1,"matched_count":2,"expected_failure":true,"test_in_source":true,"red_passed":false,"fresh_build":true,"output_digest":"b"}`)
			}
		}
		j := drive(t, newJob(t), engine.NewScripted(script), true)
		if j.EscalationKind == domain.SameGate || j.State != domain.Escalated {
			t.Fatal("alternating reasons counted as the same gate", j.EscalationKind, j.Reason)
		}
	})
	t.Run("IntegrationConflict when integration remediation is exhausted", func(t *testing.T) {
		doc := defaults.Workflow()
		doc.Limits.Attempts, doc.Limits.TestRuns = 100, 100
		j, e := engine.NewLive("landing", 1, doc, "reviewed")
		if e != nil {
			t.Fatal(e)
		}
		j.Base = "old-base"
		merges, moves := 0, 0
		j = liveDrive(t, j, engine.NewScripted(padded(12)), func(d *engine.Dispatch, r *engine.Result) {
			if d.Stage == "integrate" {
				r.Class = "target-moved"
				moves++
			}
			if d.Stage == "merge" && moves > merges {
				merges++
				r.NewTree, r.NewBase = "union-"+string(rune('a'+merges)), "tip-"+string(rune('a'+merges))
			}
		})
		assertKind(t, j, domain.IntegrationConflict, "integration-retry-loop exhausted 1 iterations")
	})
	t.Run("ResumeEvidence when protected evidence cannot be revalidated", func(t *testing.T) {
		j := newJob(t)
		executor := engine.NewScripted(defaults.HappyScript())
		for j.State != "commit" {
			var d *engine.Dispatch
			var e error
			if j, d, _, e = engine.Prepare(j); e != nil {
				t.Fatal(e)
			}
			if d != nil {
				r, _ := executor.Execute(context.Background(), *d)
				if j, _, e = engine.Complete(j, d.ID, r); e != nil {
					t.Fatal(e)
				}
			}
		}
		last := ""
		for _, e := range j.History {
			if e.Kind == "dispatch" {
				last = e.DispatchID
			}
		}
		j.History = append(j.History, engine.Event{Kind: "integration-target-moved", Stage: "integrate", DispatchID: last, Attempt: 1, Cause: "fixture"})
		j, _, _, _ = engine.Prepare(j)
		assertKind(t, j, domain.ResumeEvidence, "ProtectedEvidence")
	})
	t.Run("ExternalEffectRefused outside a live Normal disposition", func(t *testing.T) {
		doc := defaults.Workflow()
		doc.Artifacts["Receipt"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
		planner := doc.Roles["planner"]
		planner.Outputs = append(planner.Outputs, "Receipt")
		planner.Profile.Effects = []string{"fixture"}
		doc.Roles["planner"] = planner
		doc.Stages["external"] = workflow.Stage{Primitive: workflow.ExternalEffect, Role: "planner", NormalOnly: true, Subject: "fixture", Pins: map[string]string{"draft": "true"}, IdempotenceKey: "fixture", Output: "Receipt"}
		for n, e := range doc.Edges {
			if e.To == "Done" {
				doc.Edges[n].To = "external"
			}
		}
		doc.Edges = append(doc.Edges, workflow.Edge{From: "external", To: "Done"})
		j, e := engine.New("effect", 1, doc, "tree")
		if e != nil {
			t.Fatal(e)
		}
		assertKind(t, drive(t, j, engine.NewScripted(defaults.HappyScript()), true), domain.ExternalEffectRefused, "UnsupportedExternalEffect")
	})
	t.Run("OperatorEscalate and restart clears the kind", func(t *testing.T) {
		j, _, _, _ := engine.Prepare(newJob(t))
		j, _, e := engine.ApplyControl(j, engine.Control{Method: "Escalate", Reason: "needs owner"}, time.Now())
		if e != nil {
			t.Fatal(e)
		}
		assertKind(t, j, domain.OperatorEscalate, "needs owner")
		j, _, e = engine.ApplyControl(j, engine.Control{Method: "RestartWithInformation", Information: "more"}, time.Now())
		if e != nil || j.EscalationKind != "" || j.EscalationDetail != "" || engine.Validate(j) != nil {
			t.Fatal("restart kept a stale kind", e, j.EscalationKind)
		}
	})
	t.Run("live typed refusals", func(t *testing.T) {
		for class, kind := range map[string]domain.EscalationKind{engine.ClassStaleResidue: domain.StaleBranchResidue, engine.ClassRejectedChange: domain.StaleBranchResidue, engine.ClassOwnHunkLoss: domain.MergeOwnHunkLoss} {
			j, e := engine.NewLive("refusal", 1, defaults.Workflow(), "tree")
			if e != nil {
				t.Fatal(e)
			}
			stage := "intake"
			if class == engine.ClassOwnHunkLoss {
				stage = "merge"
			}
			j = liveDrive(t, j, engine.NewScripted(defaults.HappyScript()), func(d *engine.Dispatch, r *engine.Result) {
				if d.Stage == stage {
					r.Class, r.Output, r.Detail = class, nil, "lost: answer.go"
				}
			})
			assertKind(t, j, kind, "lost: answer.go")
			if class == engine.ClassStaleResidue && j.Attempts != 0 {
				t.Fatal("never-launched residue refusal charged an attempt", j.Attempts)
			}
		}
		j, _ := engine.NewLive("forged", 1, defaults.Workflow(), "tree")
		j, _, _, _ = engine.Prepare(j)
		j, d, _, _ := engine.Prepare(j)
		r := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now(), Class: engine.ClassOwnHunkLoss}
		if _, _, e := engine.Complete(j, d.ID, r); e == nil {
			t.Fatal("own-hunk loss accepted outside Merge")
		}
		r.Class, r.NewTree = engine.ClassStaleResidue, "other"
		if _, _, e := engine.Complete(j, d.ID, r); e == nil {
			t.Fatal("residue refusal accepted a tree mutation")
		}
	})
	t.Run("invalid or misplaced kinds fail validation", func(t *testing.T) {
		j := newJob(t)
		j.EscalationKind = domain.LoopCap
		if engine.Validate(j) == nil {
			t.Fatal("kind on a non-escalated task")
		}
		j.State = domain.Escalated
		j.EscalationKind = "DispatchRecoveryCap"
		if engine.Validate(j) == nil {
			t.Fatal("open kind accepted")
		}
	})
}

// A legacy untyped AttemptCap escalation still resumes after a raised limit.
func TestLegacyUntypedAttemptCapStillResumes(t *testing.T) {
	j, _ := firstDispatch(t, newJob(t))
	j.InFlight = nil
	j.State = domain.Escalated
	j.Reason = "AttemptCap"
	j.AttemptCapStage = "intake"
	j, _, e := engine.ApplyControl(j, engine.Control{Method: "SetAttemptLimit", AttemptLimit: 30}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	if j, _, e = engine.ApplyControl(j, engine.Control{Method: "Resume"}, time.Now()); e != nil || j.State != "intake" {
		t.Fatal(e, j.State)
	}
}

func TestProviderUnavailableBoundsRouteExhaustion(t *testing.T) {
	j, e := engine.NewLive("routes", 1, defaults.Workflow(), "tree")
	if e != nil {
		t.Fatal(e)
	}
	j, _, _, _ = engine.Prepare(j)
	none := engine.Options{SelectRoute: func([]workflow.Binding) (workflow.Binding, bool) { return workflow.Binding{}, false }, RouteExhaustion: time.Hour}
	j, d, events, e := engine.PrepareWith(j, none)
	if e != nil || d != nil || !j.Parked || j.Reason != "all-routes-unavailable" || j.RoutesUnavailableSince.IsZero() || len(events) != 1 || events[0].Attempt < 1 {
		t.Fatal(j.Reason, events, e)
	}
	since := j.RoutesUnavailableSince
	j = roundTrip(t, j)
	if j, d, events, e = engine.PrepareWith(j, none); e != nil || d != nil || len(events) != 0 || !j.RoutesUnavailableSince.Equal(since) {
		t.Fatal("re-park changed the durable bound", events, e)
	}
	// A healed route's dispatch keeps the bound until its provider runs
	// (Appendix B: "no route could launch"); a turn whose provider ran, here
	// with malformed output, resets it.
	healed := engine.Options{SelectRoute: func(routes []workflow.Binding) (workflow.Binding, bool) { return routes[0], true }}
	k, kd, _, e := engine.PrepareWith(j, healed)
	if e != nil || kd == nil || !k.RoutesUnavailableSince.Equal(since) {
		t.Fatal("a healed route's dispatch reset the exhaustion clock before its provider ran", e)
	}
	ran, _, e := engine.Complete(k, kd.ID, engine.Result{ExecutionKind: "live", DispatchID: kd.ID, TaskID: kd.TaskID, Tree: kd.Tree, Session: kd.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: engine.ClassMalformedOutput, Detail: "not JSON"})
	if e != nil || !ran.RoutesUnavailableSince.IsZero() {
		t.Fatal("a turn whose provider ran kept the exhaustion clock", e)
	}
	j.RoutesUnavailableSince = time.Now().Add(-2 * time.Hour)
	j, d, _, e = engine.PrepareWith(j, none)
	if e != nil || d != nil {
		t.Fatal(e)
	}
	assertKind(t, j, domain.ProviderUnavailable, "no eligible route for role spec")
	if j.Attempts != 0 || j.Sequence != 0 {
		t.Fatal("route exhaustion consumed budget")
	}
}

// Census: random per-stage faults never escalate without a closed kind.
func TestRandomFaultsAlwaysEscalateWithATypedKind(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	stages := []string{"intake", "spec", "plan", "test-first", "run-red", "implement", "run-tests", "format", "oracle", "qa", "post-merge-qa", "merge", "integrate"}
	faults := []json.RawMessage{json.RawMessage(`{"unexpected":true}`), json.RawMessage(`{"accepted":false}`), json.RawMessage(`{"decision":"unknown","valid_blocking":1,"rationale":"x"}`), json.RawMessage(`{"exit_code":1,"executed_count":0,"matched_count":0,"expected_failure":false,"test_in_source":false,"red_passed":false,"fresh_build":true,"output_digest":"x"}`), json.RawMessage(`{"new_debt":1}`), json.RawMessage(`{"result":"clean","patch_identical":false}`), json.RawMessage(`{"landed":false,"sha":"other"}`)}
	kinds := map[domain.EscalationKind]int{}
	for i := 0; i < 80; i++ {
		script := padded(10)
		for n := 0; n < 1+rng.Intn(3); n++ {
			stage := stages[rng.Intn(len(stages))]
			fault := faults[rng.Intn(len(faults))]
			for v := range script[stage] {
				if rng.Intn(2) == 0 {
					script[stage][v].Output = fault
				}
			}
		}
		j := drive(t, newJob(t), engine.NewScripted(script), i%8 == 0)
		if j.State == domain.Escalated {
			if !j.EscalationKind.Valid() || !strings.HasPrefix(j.Reason, string(j.EscalationKind)) || engine.Validate(j) != nil {
				t.Fatal("untyped escalation", i, j.Reason)
			}
			kinds[j.EscalationKind]++
		} else if j.State != domain.Done {
			t.Fatal("unexpected resting state", j.State, j.Reason)
		}
	}
	if len(kinds) < 2 {
		t.Fatal("census did not exercise several kinds", kinds)
	}
	t.Log(kinds)
}

// FAE-RT-010 / FAE-ROB-017: one free rotation per turn to the next eligible
// route; a second provider failure of the same turn is a bounded stage failure.
func TestProviderFailoverRotatesOncePerTurn(t *testing.T) {
	doc := defaults.Workflow()
	for name, role := range doc.Roles {
		role.Routes = []workflow.Binding{{Provider: "p", Model: "one"}, {Provider: "p", Model: "two"}, {Provider: "p", Model: "three"}}
		doc.Roles[name] = role
	}
	j, e := engine.NewLive("failover", 1, doc, "tree")
	if e != nil {
		t.Fatal(e)
	}
	j, _, _, _ = engine.Prepare(j)
	healthy := map[string]bool{"one": true, "two": true, "three": true}
	options := engine.Options{SelectRoute: func(routes []workflow.Binding) (workflow.Binding, bool) {
		for _, r := range routes {
			if healthy[r.Model] {
				return r, true
			}
		}
		return workflow.Binding{}, false
	}}
	throttle := func(j engine.Job) (engine.Job, *engine.Dispatch) {
		t.Helper()
		j, d, _, e := engine.PrepareWith(j, options)
		if e != nil || d == nil {
			t.Fatal(e)
		}
		healthy[d.Sessions[0].Route.Model] = false
		r := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now(), Class: engine.ClassProviderFailover, Detail: "rate-limited: 429"}
		if j, _, e = engine.Complete(j, d.ID, r); e != nil {
			t.Fatal(e)
		}
		return roundTrip(t, j), d
	}
	before := j.Attempts
	j, first := throttle(j)
	if j.InFlight != nil || j.Parked || j.Failures["failover:intake"] != 1 || j.Attempts != before || j.History[len(j.History)-1].Kind != "failover" {
		t.Fatal("first throttle did not rotate free", j.Failures, j.Attempts)
	}
	j, second := throttle(j)
	if first.Sessions[0].Route.Model != "one" || second.Sessions[0].Route.Model != "two" {
		t.Fatal("rotation did not take the next eligible route")
	}
	sameReason := 0
	for key, n := range j.Failures {
		if strings.HasPrefix(key, "same-gate:intake:") {
			sameReason += n
		}
	}
	if sameReason != 1 || j.Failures["failover:intake"] != 0 || !strings.HasPrefix(j.Reason, "ProviderFailure: ") {
		t.Fatal("exhausted rotation not a bounded stage failure", j.Failures, j.Reason)
	}
	k, d, _, e := engine.PrepareWith(j, options)
	if e != nil || d == nil || d.Sessions[0].Route.Model != "three" {
		t.Fatal("next turn did not get a fresh rotation on the remaining route", e)
	}
	r := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now(), Class: engine.ClassProviderFailover, NewTree: "edited"}
	if _, _, e = engine.Complete(k, d.ID, r); e == nil {
		t.Fatal("failover accepted a tree-changing result")
	}
}

// FAE-ROB-002/-017, Appendix B: a stage failure repeats SameGate only for the
// same stage and the same reason. The fourth identical failure escalates (the
// Appendix A bound of 3, as for gates); a new reason or a success restarts it.
func TestStageFailureSameGateSemantics(t *testing.T) {
	bad := engine.ScriptStep{Output: json.RawMessage(`{"accepted":"yes"}`)}
	other := engine.ScriptStep{Output: json.RawMessage(`{"accepted":true,"undeclared":1}`)}
	crash := engine.ScriptStep{Class: "execution-error"}
	good := defaults.HappyScript()["intake"][0]
	run := func(steps ...engine.ScriptStep) engine.Job {
		script := padded(12)
		script["intake"] = steps
		return drive(t, newJob(t), engine.NewScripted(script), true)
	}
	assertKind(t, run(bad, bad, bad, bad), domain.SameGate, "intake failed 4 times for the same reason: MalformedOutput")
	assertKind(t, run(crash, crash, crash, crash), domain.SameGate, "intake failed 4 times for the same reason: ExecutionFailure: execution-error")
	for name, steps := range map[string][]engine.ScriptStep{
		"three identical":     {bad, bad, bad, good},
		"alternating reasons": {bad, other, bad, other, bad, other, crash, crash, crash, good},
	} {
		j := run(steps...)
		if j.State != domain.Done {
			t.Fatal(name, "escalated", j.State, j.Reason)
		}
		for key := range j.Failures {
			if strings.HasPrefix(key, "same-gate:intake:") {
				t.Fatal(name, "success did not restart the same-gate count", j.Failures)
			}
		}
	}
}

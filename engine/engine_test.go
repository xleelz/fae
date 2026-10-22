package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func roundTrip(t *testing.T, j engine.Job) engine.Job {
	t.Helper()
	b, e := json.Marshal(j)
	if e != nil {
		t.Fatal(e)
	}
	var k engine.Job
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if e = dec.Decode(&k); e != nil {
		t.Fatal(e)
	}
	return k
}
func drive(t *testing.T, j engine.Job, executor engine.Executor, restart bool) engine.Job {
	t.Helper()
	for i := 0; i < 150; i++ {
		if j.State.Terminal() || j.State == domain.Escalated || j.Parked {
			return j
		}
		next, d, _, e := engine.Prepare(j)
		if e != nil {
			t.Fatal(e)
		}
		j = next
		if restart {
			j = roundTrip(t, j)
			if d != nil {
				var replay *engine.Dispatch
				j, replay, e = engine.Recover(j, true, true)
				if e != nil || replay == nil || replay.ID != d.ID {
					t.Fatal("mid-stage recovery", e)
				}
				d = replay
			}
		}
		if d != nil {
			r, e := executor.Execute(context.Background(), *d)
			if e != nil {
				t.Fatal(e)
			}
			j, _, e = engine.Complete(j, d.ID, r)
			if e != nil {
				t.Fatal(e)
			}
			if restart {
				j = roundTrip(t, j)
			}
		}
	}
	t.Fatal("unbounded")
	return j
}
func newJob(t *testing.T) engine.Job {
	t.Helper()
	j, e := engine.New("task", 1, defaults.Workflow(), "fixture-tree")
	if e != nil {
		t.Fatal(e)
	}
	return j
}
func TestDefaultPipelineAndEveryBoundaryResume(t *testing.T) {
	j := drive(t, newJob(t), engine.NewScripted(defaults.HappyScript()), true)
	if j.State != domain.Done {
		t.Fatalf("state %s reason %s", j.State, j.Reason)
	}
	if len(j.Evidence) != 22 {
		t.Fatalf("artifacts %d", len(j.Evidence))
	}
	if j.Attempts != 1 {
		t.Fatalf("attempts %d", j.Attempts)
	}
	for _, e := range j.History {
		if e.Attempt < 1 || e.Cause == "" {
			t.Fatal("missing context", e)
		}
		if e.Gate != nil && e.Gate.Passed && len(e.Gate.EvidenceIDs) == 0 {
			t.Fatal("vacuous gate")
		}
	}
	k, d, ev, e := engine.Prepare(j)
	if e != nil || d != nil || len(ev) != 0 || k.State != domain.Done {
		t.Fatal("terminal dispatched")
	}
	if _, _, e = engine.Complete(j, "old", engine.Result{}); e == nil {
		t.Fatal("terminal completion")
	}
}
func TestDefaultContentStableAndIndependent(t *testing.T) {
	a := defaults.Workflow()
	b := defaults.Workflow()
	ha, _ := workflow.Hash(a)
	hb, _ := workflow.Hash(b)
	if ha != hb {
		t.Fatal("unstable hash")
	}
	a.Roles["spec"] = workflow.Role{}
	if b.Roles["spec"].Cap != 2 {
		t.Fatal("aliased")
	}
}
func TestCompletionIdentityRefusals(t *testing.T) {
	j := newJob(t)
	j, _, _, _ = engine.Prepare(j)
	j, d, _, e := engine.Prepare(j)
	if e != nil {
		t.Fatal(e)
	}
	r, e := engine.NewScripted(defaults.HappyScript()).Execute(context.Background(), *d)
	if e != nil {
		t.Fatal(e)
	}
	mutations := []func(*engine.Result){func(r *engine.Result) { r.DispatchID = "old" }, func(r *engine.Result) { r.TaskID = "other" }, func(r *engine.Result) { r.Tree = "stale" }, func(r *engine.Result) { r.Session = "producer" }, func(r *engine.Result) { r.ProducedAt = d.CreatedAt.Add(-time.Second) }}
	for _, mut := range mutations {
		bad := r
		mut(&bad)
		if _, _, e := engine.Complete(j, d.ID, bad); e == nil {
			t.Fatal("accepted forged envelope")
		}
	}
	k, _, e := engine.Complete(j, d.ID, r)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = engine.Complete(k, d.ID, r); e == nil {
		t.Fatal("accepted duplicate")
	}
}
func TestMalformedOutputRetriesAndEscalates(t *testing.T) {
	script := defaults.HappyScript()
	script["intake"] = []engine.ScriptStep{{Output: json.RawMessage(`{"accepted":"yes"}`)}, {Output: json.RawMessage(`{"accepted":true,"undeclared":1}`)}, {Output: json.RawMessage(`{"accepted":true,"accepted":false}`)}}
	j := drive(t, newJob(t), engine.NewScripted(script), true)
	if j.State != domain.Escalated || j.Failures["intake"] != 3 || !strings.Contains(j.Reason, "MalformedOutput") {
		t.Fatal(j.State, j.Reason, j.Failures)
	}
}
func TestMissingInputParksWithoutDispatch(t *testing.T) {
	j := newJob(t)
	j.State = "implement"
	j, d, _, e := engine.Prepare(j)
	if e != nil || d != nil || !j.Parked || !strings.Contains(j.Reason, "frozen") {
		t.Fatal(j.Reason, e)
	}
}
func TestUncertaintyAndExplicitRecovery(t *testing.T) {
	j := newJob(t)
	j, _, _, _ = engine.Prepare(j)
	j, d, _, _ := engine.Prepare(j)
	k, replay, e := engine.Recover(j, true, true)
	if e != nil || replay.ID != d.ID || k.Attempts != j.Attempts {
		t.Fatal(e)
	}
	k, replay, e = engine.Recover(j, false, false)
	if e != nil || replay != nil || !k.Parked {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		j, _, e = engine.Recover(j, false, true)
		if e != nil {
			t.Fatal(e)
		}
	}
	if j.State != domain.Escalated || j.Reason != "DispatchRecoveryCap" {
		t.Fatal(j.State, j.Reason)
	}
}
func TestWorkflowAndArtifactTamperingRejected(t *testing.T) {
	j := newJob(t)
	j.Document.Limits.Attempts++
	if _, _, _, e := engine.Prepare(j); e == nil {
		t.Fatal("binding mutation")
	}
	j = drive(t, newJob(t), engine.NewScripted(defaults.HappyScript()), false)
	j.Evidence[0].TaskID = "foreign"
	if e := engine.Validate(j); e == nil {
		t.Fatal("foreign artifact")
	}
}
func TestFrozenChangeAndZeroTestsCannotCommit(t *testing.T) {
	for _, which := range []string{"frozen", "zero", "qa", "review"} {
		t.Run(which, func(t *testing.T) {
			script := defaults.HappyScript()
			stage := "implement"
			if which == "zero" {
				stage = "run-tests"
			}
			if which == "qa" {
				stage = "qa"
			}
			if which == "review" {
				stage = "reviews"
			}
			if which == "review" {
				for i := range script[stage][0].Reviews {
					var x map[string]any
					json.Unmarshal(script[stage][0].Reviews[i], &x)
					x["approved"] = false
					script[stage][0].Reviews[i], _ = json.Marshal(x)
				}
			} else {
				var x map[string]any
				json.Unmarshal(script[stage][0].Output, &x)
				switch which {
				case "frozen":
					x["source"] = "package example\nfunc TestBehavior(t *T) { assert(true) }"
				case "zero":
					x["executed_count"] = 0
				case "qa":
					x["exit_code"] = 1
				}
				script[stage][0].Output, _ = json.Marshal(x)
			}
			// Repeated fixtures exercise bounded remediation rather than script exhaustion.
			for key, steps := range script {
				for len(steps) < 10 {
					steps = append(steps, steps[0])
				}
				script[key] = steps
			}
			j := drive(t, newJob(t), engine.NewScripted(script), true)
			if j.State == domain.Done {
				t.Fatal("unsafe run completed")
			}
			for _, e := range j.History {
				if e.Kind == "dispatch" && e.Stage == "commit" {
					t.Fatal("unsafe commit dispatched")
				}
			}
		})
	}
}

func TestCodeBearingMutationCorpus(t *testing.T) {
	for _, kind := range []string{"skip-red", "skip-freeze", "skip-compare", "wrong-red", "mutate-after-compare"} {
		t.Run(kind, func(t *testing.T) {
			d := defaults.Workflow()
			switch kind {
			case "skip-red":
				d.Edges = append(d.Edges, workflow.Edge{From: "test-first", To: "implement"})
			case "skip-freeze":
				d.Edges = append(d.Edges, workflow.Edge{From: "red-gate", To: "implement"})
			case "skip-compare":
				d.Edges = append(d.Edges, workflow.Edge{From: "run-tests", To: "green-gate"})
			case "wrong-red":
				s := d.Stages["red-gate"]
				x := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "run-red", Path: []string{"exit_code"}}, {Op: "literal", Value: 0}}}
				s.Expr = &x
				d.Stages["red-gate"] = s
			case "mutate-after-compare":
				s := d.Stages["format"]
				s.Phase = "implement"
				d.Stages["format"] = s
			}
			if len(workflow.Validate(d)) == 0 {
				t.Fatal("accepted unsafe code-bearing path")
			}
		})
	}
}
func TestReviewDimensionsCannotCollapse(t *testing.T) {
	s := defaults.HappyScript()
	for i := range s["reviews"][0].Reviews {
		s["reviews"][0].Reviews[i] = s["reviews"][0].Reviews[0]
	}
	for k, steps := range s {
		for len(steps) < 10 {
			steps = append(steps, steps[0])
		}
		s[k] = steps
	}
	j := drive(t, newJob(t), engine.NewScripted(s), false)
	if j.State == domain.Done {
		t.Fatal("collapsed review dimensions passed")
	}
}
func TestRedFailureRoutingAndOracleTargets(t *testing.T) {
	for _, which := range []string{"red-failure", "Production", "RedTest", "unknown"} {
		t.Run(which, func(t *testing.T) {
			script := defaults.HappyScript()
			for k, steps := range script {
				for len(steps) < 10 {
					steps = append(steps, steps[0])
				}
				script[k] = steps
			}
			if which == "red-failure" {
				var fields map[string]any
				json.Unmarshal(script["run-red"][0].Output, &fields)
				fields["expected_failure"] = false
				script["run-red"][0].Output, _ = json.Marshal(fields)
			} else {
				script["oracle"][0].Output = []byte(`{"decision":"` + which + `","valid_blocking":1,"rationale":"rework"}`)
			}
			j := drive(t, newJob(t), engine.NewScripted(script), true)
			if which == "unknown" {
				if j.State != domain.Escalated {
					t.Fatal(j.State)
				}
				return
			}
			target := "implement-loop"
			if which == "red-failure" || which == "RedTest" {
				target = "red-loop"
			}
			found := false
			for _, e := range j.History {
				if (e.From == "red-gate" || e.From == "verdict") && string(e.To) == target {
					found = true
				}
			}
			if !found {
				t.Fatal("did not route rework", which)
			}
			if which == "red-failure" && j.State != domain.Done {
				t.Fatal("corrected RED did not complete", j.State, j.Reason)
			}
			if which == "RedTest" {
				n := 0
				for _, a := range j.Evidence {
					if a.Type == "FrozenIdentity" {
						n++
					}
				}
				if n < 2 {
					t.Fatal("superseded frozen identity lost")
				}
			}
		})
	}
}

func TestGeneratedDurableWorkflowParity10000(t *testing.T) {
	rng := rand.New(rand.NewSource(60161))
	schema := json.RawMessage(`{"type":"object","required":["ok"],"additionalProperties":false,"properties":{"ok":{"type":"boolean"}}}`)
	for iteration := 0; iteration < 10000; iteration++ {
		size := 1 + rng.Intn(3)
		want := rng.Intn(4) != 0
		d := workflow.Document{SchemaVersion: 1, ID: fmt.Sprintf("generated-%d", iteration), Start: "s0", Limits: workflow.Limits{Attempts: 6, TestRuns: 2, ScopeWidens: 1}, Roles: map[string]workflow.Role{"worker": {Instructions: "Fixture", Briefs: map[string]string{"main": "fixture"}, Routes: []workflow.Binding{{Provider: "scripted", Model: "fixture"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"Report"}}}, Artifacts: map[string]workflow.ArtifactType{"Report": {Version: 1, Schema: schema}}, Stages: map[string]workflow.Stage{}}
		script := map[string][]engine.ScriptStep{}
		for i := 0; i < size; i++ {
			id := fmt.Sprintf("s%d", i)
			s := workflow.Stage{Primitive: workflow.AgentTurn, Role: "worker", Output: "Report"}
			if i > 0 {
				s.Inputs = []workflow.Input{{Name: "prior", Type: "Report", Stage: fmt.Sprintf("s%d", i-1), Version: 1}}
			}
			d.Stages[id] = s
			next := fmt.Sprintf("s%d", i+1)
			if i == size-1 {
				next = "gate"
			}
			d.Edges = append(d.Edges, workflow.Edge{From: id, To: next})
			script[id] = []engine.ScriptStep{{Output: json.RawMessage(fmt.Sprintf(`{"ok":%t}`, want))}}
		}
		expression := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: fmt.Sprintf("s%d", size-1), Path: []string{"ok"}}, {Op: "literal", Value: true}}}
		d.Stages["gate"] = workflow.Stage{Primitive: workflow.Gate, Expr: &expression, Failure: "stop"}
		d.Stages["stop"] = workflow.Stage{Primitive: workflow.Escalate}
		d.Edges = append(d.Edges, workflow.Edge{From: "gate", To: "Done"}, workflow.Edge{From: "gate", To: "stop"})
		initial, e := engine.New("task", 1, d, "tree")
		if e != nil {
			t.Fatal(e)
		}
		resumed := drive(t, initial, engine.NewScripted(script), true)
		expected := domain.Done
		if !want {
			expected = domain.Escalated
		}
		if resumed.State != expected {
			t.Fatal(iteration, resumed.State, expected)
		}
		if resumed.Sequence != size {
			t.Fatal("duplicate completed dispatch", iteration)
		}
		for _, event := range resumed.History {
			if event.Kind == "transition" && (event.Attempt < 1 || event.Cause == "") {
				t.Fatal("missing audit context")
			}
			if event.Gate != nil && event.Gate.Passed && len(event.Gate.EvidenceIDs) == 0 {
				t.Fatal("vacuous gate")
			}
		}
	}
}

func TestAttemptBudgetCountsRetriesNotPipelineLength(t *testing.T) {
	d := defaults.Workflow()
	d.Limits.Attempts = 1
	j, e := engine.New("task", 1, d, "tree")
	if e != nil {
		t.Fatal(e)
	}
	j = drive(t, j, engine.NewScripted(defaults.HappyScript()), true)
	if j.State != domain.Done || j.Attempts != 1 || j.Sequence != 18 {
		t.Fatal(j.State, j.Attempts, j.Sequence)
	}
	script := defaults.HappyScript()
	script["intake"][0].Output = json.RawMessage(`{"accepted":"invalid"}`)
	j, e = engine.New("task", 1, d, "tree")
	if e != nil {
		t.Fatal(e)
	}
	j = drive(t, j, engine.NewScripted(script), true)
	if j.State != domain.Escalated || j.Reason != "AttemptCap" || j.Attempts != 1 {
		t.Fatal(j.State, j.Reason, j.Attempts)
	}
}
func TestReviewInputsExcludeSiblingOutputs(t *testing.T) {
	j := newJob(t)
	executor := engine.NewScripted(defaults.HappyScript())
	for i := 0; i < 100; i++ {
		next, d, _, e := engine.Prepare(j)
		if e != nil {
			t.Fatal(e)
		}
		j = next
		if d == nil {
			continue
		}
		if d.Stage == "reviews" {
			if len(d.Sessions) != 4 {
				t.Fatal("missing dimensions")
			}
			seen := map[string]bool{}
			for _, s := range d.Sessions {
				if seen[s.ID] {
					t.Fatal("shared session")
				}
				seen[s.ID] = true
			}
			for _, a := range d.Inputs {
				if a.Type == "Review" || a.Type == "SpecReview" {
					t.Fatal("sibling review disclosed")
				}
			}
			return
		}
		r, e := executor.Execute(context.Background(), *d)
		if e != nil {
			t.Fatal(e)
		}
		j, _, e = engine.Complete(j, d.ID, r)
		if e != nil {
			t.Fatal(e)
		}
	}
	t.Fatal("reviews not reached")
}

func TestScriptedEvidenceKindPersistsAndCannotRelabel(t *testing.T) {
	j := roundTrip(t, drive(t, newJob(t), engine.NewScripted(defaults.HappyScript()), true))
	if j.ExecutionKind != "scripted" {
		t.Fatal("missing rehearsal marker")
	}
	for _, p := range j.Provenance {
		if p.ExecutionKind != "scripted" {
			t.Fatal("unmarked artifact")
		}
	}
	j.Provenance[0].ExecutionKind = "live"
	if engine.Validate(j) == nil {
		t.Fatal("relabelled evidence accepted")
	}
	j.Provenance[0].ExecutionKind = "scripted"
	j.ExecutionKind = "live"
	if engine.Validate(j) == nil {
		t.Fatal("relabelled job accepted")
	}
}

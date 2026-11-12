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

func newPrimitiveJob(t *testing.T, d workflow.Document) engine.Job {
	t.Helper()
	j, e := engine.New("task", 1, d, "tree")
	if e != nil {
		t.Fatal(e)
	}
	return j
}
func reviewStep(outputs ...string) engine.ScriptStep {
	step := engine.ScriptStep{}
	for _, o := range outputs {
		step.Reviews = append(step.Reviews, json.RawMessage(o))
	}
	return step
}
func recordsOf(j engine.Job, kind string) []workflow.GateRecord {
	out := []workflow.GateRecord{}
	for _, r := range j.GateRecords {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// FAE-WF-030 Review aggregation conformance: each rule's aggregate verdict is
// recorded and routes pass to the success route, fail or unknown to failure.
func TestReviewAggregationRules(t *testing.T) {
	cases := []struct {
		rule    string
		quorum  int
		reviews []string
		pass    bool
	}{
		{workflow.AllPass, 0, []string{`{"approved":true}`, `{"approved":true}`, `{"approved":true}`}, true},
		{workflow.AllPass, 0, []string{`{"approved":true}`, `{"approved":false}`, `{"approved":true}`}, false},
		{workflow.KOfN, 2, []string{`{"approved":true}`, `{"approved":false}`, `{"approved":true}`}, true},
		{workflow.KOfN, 2, []string{`{"approved":false}`, `{"approved":false}`, `{"approved":true}`}, false},
		{workflow.KOfN, 2, []string{`{"approved":true}`, `{}`, `{"approved":true}`}, false},
		{workflow.AnyBlockingFails, 0, []string{`{"approved":true,"findings":[{"severity":"non-blocking"}]}`, `{"approved":true,"findings":[]}`, `{"approved":true,"findings":[]}`}, true},
		{workflow.AnyBlockingFails, 0, []string{`{"approved":true,"findings":[{"severity":"non-blocking"},{"severity":"blocking"}]}`, `{"approved":true,"findings":[]}`, `{"findings":[]}`}, false},
		{workflow.AnyBlockingFails, 0, []string{`{"approved":true,"findings":[{"severity":"cosmetic"}]}`, `{"findings":[]}`, `{"findings":[]}`}, false},
		{workflow.AnyBlockingFails, 0, []string{`{"approved":true}`, `{"findings":[]}`, `{"findings":[]}`}, false},
	}
	for i, c := range cases {
		d := floorDoc()
		r := d.Stages["review"]
		r.Aggregation, r.Quorum, r.Count, r.Failure = c.rule, c.quorum, 3, "stop"
		d.Stages["review"] = r
		d.Edges = append(d.Edges, workflow.Edge{From: "review", To: "stop"})
		script := floorScript(1)
		script["review"] = []engine.ScriptStep{reviewStep(c.reviews...)}
		j := drive(t, newPrimitiveJob(t, d), engine.NewScripted(script), true)
		aggregates := recordsOf(j, workflow.RecordAggregate)
		if len(aggregates) != 1 || aggregates[0].Result.Passed != c.pass || (j.State == domain.Done) != c.pass {
			t.Fatalf("case %d %s: state %s records %v", i, c.rule, j.State, aggregates)
		}
		if !c.pass && j.EscalationKind != domain.HumanDecisionRequired {
			t.Fatalf("case %d: failure route not taken: %s", i, j.Reason)
		}
		if err := engine.ReplayGates(roundTrip(t, j)); err != nil {
			t.Fatal(err)
		}
	}
}

// FAE-WF-030 HostCommand expected-shape conformance: a run whose engine-run
// artifact contradicts the declared shape takes the failure route; the
// environment policy reaches the dispatched stage configuration.
func TestHostCommandExpectedShapeAndEnvironment(t *testing.T) {
	d := floorDoc()
	h := d.Stages["test"]
	h.ExpectedShape, h.Failure, h.Environment = &workflow.CommandShape{Exit: "zero", Matched: 1}, "stop", map[string]string{"GOFLAGS": "-mod=mod"}
	d.Stages["test"] = h
	d.Edges = append(d.Edges, workflow.Edge{From: "test", To: "stop"})
	for _, c := range []struct {
		output string
		pass   bool
	}{{`{"exit_code":0,"executed_count":1,"matched_count":1,"fresh_build":true}`, true}, {`{"exit_code":0,"executed_count":2,"matched_count":2,"fresh_build":true}`, false}, {`{"exit_code":0,"executed_count":1,"fresh_build":true}`, false}} {
		script := floorScript(1)
		script["test"] = step(c.output)
		j := newPrimitiveJob(t, d)
		var dispatched *engine.Dispatch
		for i := 0; i < 3 && dispatched == nil; i++ {
			j, dispatched, _, _ = engine.Prepare(j)
			if dispatched != nil && dispatched.Stage != "test" {
				r, e := engine.NewScripted(script).Execute(t.Context(), *dispatched)
				if e != nil {
					t.Fatal(e)
				}
				if j, _, e = engine.Complete(j, dispatched.ID, r); e != nil {
					t.Fatal(e)
				}
				dispatched = nil
			}
		}
		if dispatched == nil || dispatched.Config.Environment["GOFLAGS"] != "-mod=mod" {
			t.Fatal("environment policy not dispatched", dispatched)
		}
		r, e := engine.NewScripted(script).Execute(t.Context(), *dispatched)
		if e != nil {
			t.Fatal(e)
		}
		if j, _, e = engine.Complete(j, dispatched.ID, r); e != nil {
			t.Fatal(e)
		}
		shapes := recordsOf(j, workflow.RecordShape)
		if len(shapes) != 1 || shapes[0].Result.Passed != c.pass || (j.State == "gate") != c.pass || (j.State == "stop") == c.pass {
			t.Fatalf("%s: state %s shape %v", c.output, j.State, shapes)
		}
	}
}

// FAE-WF-030 Loop and Escalate conformance: the exhaustion kind and exit gate,
// the backoff park before each repeated iteration, and the reason template.
func TestLoopExhaustionBackoffAndEscalateReason(t *testing.T) {
	d := floorDoc()
	d.Stages["again"] = workflow.Stage{Primitive: workflow.Loop, MaxIterations: 2, ExitGate: "gate", BackoffSeconds: 60, OnExhaustion: domain.TestRunCap}
	g := d.Stages["gate"]
	g.Failure, g.Name = "again", "Unit tests green"
	d.Stages["gate"] = g
	replaceEdge(&d, workflow.Edge{From: "gate", To: "stop"}, workflow.Edge{From: "gate", To: "again"}, workflow.Edge{From: "again", To: "change"})
	delete(d.Stages, "stop")
	d.Stages["halt"] = workflow.Stage{Primitive: workflow.Escalate, Kind: domain.OperatorEscalate, Reason: "{task} halted at {stage} on attempt {attempt}"}
	script := floorScript(3)
	script["test"] = nil
	for i := 0; i < 3; i++ {
		script["test"] = append(script["test"], step(`{"exit_code":1,"executed_count":1,"fresh_build":true}`)...)
	}
	// The review route can halt instead of committing (reason template).
	d.Stages["choose"] = workflow.Stage{Primitive: workflow.Branch, Default: "halt", Cases: []workflow.Case{{When: bin("eq", fieldOf("review", "ship"), lit(true)), Next: "commit"}}}
	replaceEdge(&d, workflow.Edge{From: "review", To: "commit"}, workflow.Edge{From: "review", To: "choose"}, workflow.Edge{From: "choose", To: "commit"}, workflow.Edge{From: "choose", To: "halt"})
	j := drive(t, newPrimitiveJob(t, d), engine.NewScripted(script), false)
	if j.State != "again" || !j.Parked || j.Reason != engine.ParkLoopBackoff || j.Counts["again"] != 1 {
		t.Fatal("repeated iteration must back off", j.State, j.Reason, j.Counts["again"])
	}
	if next, dispatch, events, e := engine.Prepare(j); e != nil || dispatch != nil || len(events) != 0 || !next.Parked {
		t.Fatal("backoff ended early")
	}
	j.AdmissionRetryAfter = time.Now().Add(-time.Second)
	j, _, _, e := engine.Prepare(j)
	if e != nil || j.State != "change" || j.Counts["again"] != 2 {
		t.Fatal("elapsed backoff must resume the iteration", j.State, e)
	}
	j = drive(t, j, engine.NewScripted(script), false)
	j.AdmissionRetryAfter = time.Now().Add(-time.Second)
	j = drive(t, j, engine.NewScripted(script), false)
	if j.State != domain.Escalated || j.EscalationKind != domain.TestRunCap || !strings.Contains(j.EscalationDetail, "again exhausted 2 iterations without passing exit gate gate") {
		t.Fatal("exhaustion kind and exit gate", j.EscalationKind, j.EscalationDetail)
	}
	gates := recordsOf(j, workflow.RecordGate)
	if len(gates) != 3 || gates[0].Name != "Unit tests green" || gates[0].Result.Passed {
		t.Fatal("named gate records", gates)
	}
	// Reason template: a passing run reaches the Branch, whose default halts.
	passing := floorScript(1)
	passing["review"] = []engine.ScriptStep{reviewStep(`{"approved":true,"ship":false}`)}
	j = drive(t, newPrimitiveJob(t, d), engine.NewScripted(passing), true)
	if j.EscalationKind != domain.OperatorEscalate || j.EscalationDetail != "task halted at halt on attempt 1" {
		t.Fatal("reason template", j.EscalationKind, j.EscalationDetail)
	}
	if arms := recordsOf(j, workflow.RecordBranchArm); len(arms) != 1 || arms[0].Arm != 1 || arms[0].Result.Value != workflow.False {
		t.Fatal("branch arm evaluation not recorded", arms)
	}
}

// FAE-WF-063: every gate, branch-arm and aggregate result is a separately
// content-addressed record that replays identically over the recorded
// evidence after a durable round trip; tampering is detected.
func TestGateRecordsReplayIdentically(t *testing.T) {
	j := drive(t, newJob(t), engine.NewScripted(padded(3)), true)
	if j.State != domain.Done || len(recordsOf(j, workflow.RecordGate)) < 5 || len(recordsOf(j, workflow.RecordBranchArm)) < 1 {
		t.Fatal(j.State, len(j.GateRecords))
	}
	stored := roundTrip(t, j)
	if err := engine.ReplayGates(stored); err != nil {
		t.Fatal(err)
	}
	cited := map[string]bool{}
	for _, e := range stored.History {
		if e.Record != "" {
			cited[e.Record] = true
		}
	}
	for _, r := range stored.GateRecords {
		if !cited[r.ID] {
			t.Fatal("record not cited by its event", r.ID)
		}
	}
	tampered := roundTrip(t, j)
	tampered.GateRecords[0].Result.Passed = !tampered.GateRecords[0].Result.Passed
	if engine.Validate(tampered) == nil {
		t.Fatal("tampered gate record validated")
	}
	if id, _ := tampered.GateRecords[0].Digest(); true {
		tampered.GateRecords[0].ID = id
	}
	if err := engine.ReplayGates(tampered); err == nil || !strings.Contains(err.Error(), "replays differently") {
		t.Fatal("re-addressed tampered result replayed", err)
	}
	missing := roundTrip(t, j)
	missing.Evidence = missing.Evidence[1:]
	if err := engine.ReplayGates(missing); err == nil {
		t.Fatal(fmt.Sprint("replay over missing evidence"))
	}
}

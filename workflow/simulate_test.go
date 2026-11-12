package workflow_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-091 on the simulator, which is the durable engine driven offline on
// a scripted adapter (engine.Simulate): these cases pin its path, refusals
// and records for each primitive the workflow package declares.

func simulate(d workflow.Document, task string, script workflow.Script) (workflow.Simulation, error) {
	return engine.Simulate(d, task, script, workflow.SimulationOptions{})
}
func record(stage string, fields map[string]any) []workflow.ScriptStep {
	return []workflow.ScriptStep{{Evidence: []workflow.Evidence{{ID: stage + "-1", TaskID: "t", Stage: stage, Type: "report", Version: 1, Provenance: true, Fields: fields}}}}
}

var greenRun = map[string]any{"exit_code": 0, "executed_count": 1, "fresh_build": true}

// The simulator evaluates loops, gates and branch arms, executes commit
// stages only on protected evidence, and reports the path.
func TestSimulate(t *testing.T) {
	d := workflow.MinimalDoc()
	s, e := simulate(d, "t", workflow.Script{})
	if e != nil || s.State != domain.Done || len(s.Transitions) != 2 {
		t.Fatal(s, e)
	}
	if _, e = simulate(d, "t", workflow.Script{"work": {{}}}); e == nil {
		t.Fatal("scripted loop route accepted")
	}
	d.Stages["again"] = workflow.Stage{Primitive: workflow.Branch, Default: "Done", Cases: []workflow.Case{{When: lit(true), Next: "work"}}}
	d.Edges = []workflow.Edge{{From: "work", To: "again"}, {From: "again", To: "work"}, {From: "again", To: "Done"}}
	s, e = simulate(d, "t", workflow.Script{})
	if e != nil || s.State != domain.Escalated || !strings.HasPrefix(s.Reason, "LoopCap") || strings.Join(s.Path, ",") != "work,again,work,again,work,Escalated" {
		t.Fatal(s, e)
	}
	d = workflow.ProtectedDoc()
	script := workflow.Script{"change": record("change", map[string]any{}), "test": record("test", greenRun), "review": record("review", map[string]any{"approved": true}), "commit": record("commit", map[string]any{"sha": "simulated"})}
	s, e = simulate(d, "t", script)
	if e != nil || s.State != domain.Done || strings.Join(s.Path, ",") != "change,test,gate,review,commit,Done" || len(s.Gates) != 1 || len(s.Evidence) != 4 {
		t.Fatal(s, e)
	}
	script["review"] = record("review", map[string]any{"approved": false})
	if s, e = simulate(d, "t", script); e != nil || s.State != domain.Escalated || !strings.Contains(s.Reason, "ProtectedEvidence") {
		t.Fatal("unreviewed commit simulated", s, e)
	}
	script["review"] = record("review", map[string]any{"approved": true})
	script["test"] = record("test", map[string]any{"exit_code": 0, "executed_count": 0, "fresh_build": true})
	if s, e = simulate(d, "t", script); e != nil || s.State != domain.Escalated || s.Path[len(s.Path)-2] != "stop" {
		t.Fatal("zero executed tests passed", s, e)
	}
	stale := record("test", map[string]any{"exit_code": 0, "executed_count": 1})
	script["test"] = append(append(append(stale, stale...), stale...), stale...)
	if s, e = simulate(d, "t", script); e != nil || s.State != domain.Escalated || !strings.Contains(s.Reason, "fresh build") {
		t.Fatal("stale build simulated", s, e)
	}
	script["test"] = record("test", greenRun)
	if _, e = simulate(d, "other", script); e == nil {
		t.Fatal("cross-task evidence accepted")
	}
}

func TestSimulateHumanDecision(t *testing.T) {
	d := workflow.DecisionDoc()
	script := workflow.Script{"change": append(record("change", map[string]any{}), record("change", map[string]any{"v": 2})...), "test": append(record("test", greenRun), record("test", greenRun)...), "review": append(record("review", map[string]any{"approved": true}), record("review", map[string]any{"approved": true})...), "commit": record("commit", map[string]any{"sha": "simulated"})}
	s, e := simulate(d, "t", script)
	if e != nil || s.State != "approve" || s.Reason != string(domain.HumanDecisionRequired) {
		t.Fatal("simulation did not wait for the decision", s, e)
	}
	script["approve"] = []workflow.ScriptStep{{Decision: "rework"}, {Decision: "ship"}}
	s, e = simulate(d, "t", script)
	if e != nil || s.State != domain.Done || strings.Join(s.Path, ",") != "change,test,gate,review,approve,again,change,test,gate,review,approve,commit,Done" {
		t.Fatal(s, e)
	}
	decisions := 0
	for _, a := range s.Evidence {
		if a.Stage == "approve" {
			decisions++
			if a.Role != "operator" || a.Fields["option"] == nil || a.Fields["question"] != d.Stages["approve"].Question {
				t.Fatal(a)
			}
		}
	}
	if decisions != 2 {
		t.Fatal("decision artifacts", decisions)
	}
	for name, bad := range map[string][]workflow.ScriptStep{"unknown option": {{Decision: "merge"}}, "evidence": {{Decision: "ship", Next: "commit"}}} {
		script["approve"] = bad
		if _, e = simulate(d, "t", script); e == nil {
			t.Fatal(name, "accepted")
		}
	}
	script["approve"] = []workflow.ScriptStep{{Decision: "ship"}}
	script["review"] = []workflow.ScriptStep{{Decision: "ship"}}
	if _, e = simulate(d, "t", script); e == nil {
		t.Fatal("decision scripted for a review")
	}
}

func TestSimulateConfiguredPrimitives(t *testing.T) {
	d := workflow.ConfigDoc()
	step := func(stage string, fields map[string]any) workflow.ScriptStep {
		return workflow.ScriptStep{Evidence: []workflow.Evidence{{ID: stage + "-" + fmt.Sprint(fields), TaskID: "t", Stage: stage, Type: "report", Version: 1, Provenance: true, Fields: fields}}}
	}
	reviews := func(approved ...bool) workflow.ScriptStep {
		s := workflow.ScriptStep{}
		for i, a := range approved {
			s.Evidence = append(s.Evidence, workflow.Evidence{ID: fmt.Sprintf("review-%d-%v", i, a), TaskID: "t", Stage: "review", Type: "report", Version: 1, Provenance: true, Fields: map[string]any{"approved": a}})
		}
		return s
	}
	script := workflow.Script{"change": {step("change", map[string]any{})}, "test": {step("test", greenRun)}, "review": {reviews(true, false, true)}, "commit": {step("commit", map[string]any{"sha": "s"})}}
	s, e := simulate(d, "t", script)
	if e != nil || s.State != domain.Done {
		t.Fatal("2 of 3 approvals pass k-of-n", s, e)
	}
	kinds := map[string]int{}
	for _, r := range s.Records {
		kinds[r.Kind]++
		if err := workflow.Replay(r, s.Evidence); err != nil {
			t.Fatal(err)
		}
		if r.Kind == workflow.RecordGate && r.Name != "Unit tests green" {
			t.Fatal("gate name not recorded", r.Name)
		}
	}
	if kinds[workflow.RecordGate] != 1 || kinds[workflow.RecordAggregate] != 1 || kinds[workflow.RecordShape] != 1 {
		t.Fatal("records", kinds)
	}
	script["review"] = []workflow.ScriptStep{reviews(true, false, false)}
	if s, e = simulate(d, "t", script); e != nil || s.State != domain.Escalated || s.Reason != "HumanDecisionRequired: t stopped at stop on attempt 1" {
		t.Fatal("failed aggregate must take the failure route", s.Reason, e)
	}
	script["review"] = []workflow.ScriptStep{reviews(true, true, true)}
	script["test"] = []workflow.ScriptStep{step("test", map[string]any{"exit_code": 0, "executed_count": 0, "fresh_build": true})}
	if s, e = simulate(d, "t", script); e != nil || s.State != domain.Escalated || s.Path[len(s.Path)-2] != "stop" || strings.Contains(strings.Join(s.Path, ","), "gate") {
		t.Fatal("shape mismatch must take the failure route", s.Path, e)
	}
	failing := func(exit int) workflow.ScriptStep {
		return step("test", map[string]any{"exit_code": exit, "executed_count": 1, "fresh_build": true})
	}
	script["test"] = []workflow.ScriptStep{failing(1), failing(2), failing(3), failing(4)}
	d.Stages["test"] = workflow.Stage{Primitive: workflow.HostCommand, Output: "report", Command: []string{"go", "test", "./..."}, Weight: 1, TimeoutSeconds: 30, TestRun: true}
	script["change"] = []workflow.ScriptStep{step("change", map[string]any{"v": 1}), step("change", map[string]any{"v": 2}), step("change", map[string]any{"v": 3}), step("change", map[string]any{"v": 4})}
	edges := []workflow.Edge{}
	for _, e := range d.Edges {
		if e != (workflow.Edge{From: "test", To: "stop"}) {
			edges = append(edges, e)
		}
	}
	d.Edges = edges
	if s, e = simulate(d, "t", script); e != nil || s.State != domain.Escalated || !strings.HasPrefix(s.Reason, string(domain.TestRunCap)+": again exhausted 3 iterations") {
		t.Fatal("loop exhaustion kind", s.Reason, e)
	}
	// The same gate failing for the same reason past the repeat bound is
	// SameGate, before the loop's own cap (the engine's rule, now simulated).
	script["test"] = []workflow.ScriptStep{failing(1), failing(1), failing(1), failing(1)}
	if s, e = simulate(d, "t", script); e != nil || s.State != domain.Escalated || !strings.HasPrefix(s.Reason, string(domain.SameGate)) {
		t.Fatal("same-gate repeats", s.Reason, e)
	}
}

func TestSimulateFan(t *testing.T) {
	script := workflow.Script{"change": record("change", map[string]any{}), "test": record("test", greenRun), "review": record("review", map[string]any{"approved": true}), "secure": record("secure", map[string]any{"approved": true}), "commit": record("commit", map[string]any{"sha": "s"})}
	s, e := simulate(workflow.FanDoc(), "t", script)
	if e != nil || s.State != domain.Done || strings.Join(s.Path, ",") != "change,test,gate,fan,review,join,secure,join,commit,Done" {
		t.Fatal(s.Path, e)
	}
	verdicts := 0
	for _, r := range s.Records {
		if r.Kind == workflow.RecordBranch {
			verdicts++
			if err := workflow.Replay(r, s.Evidence); err != nil || !r.Result.Passed {
				t.Fatal(r, err)
			}
		}
	}
	if verdicts != 2 {
		t.Fatal("branch verdict records", verdicts)
	}
	script["secure"] = record("secure", map[string]any{"approved": false})
	if s, e = simulate(workflow.FanDoc(), "t", script); e != nil || s.State != domain.Escalated || s.Path[len(s.Path)-2] != "stop" {
		t.Fatal("failed branch must fail an all join", s.Path, e)
	}
	d := workflow.FanDoc()
	j := d.Stages["join"]
	j.Rule = workflow.JoinAny
	d.Stages["join"] = j
	delete(script, "secure")
	if s, e = simulate(d, "t", script); e != nil || s.State != domain.Done || strings.Contains(strings.Join(s.Path, ","), "secure") {
		t.Fatal("a settled any join must skip the remaining branch", s.Path, e)
	}
}

func TestSimulateSubWorkflow(t *testing.T) {
	script := workflow.Script{"change": record("change", map[string]any{}), "test": record("test", greenRun), "review": record("review", map[string]any{"approved": true}), "commit": record("commit", map[string]any{"sha": "s"})}
	if _, e := simulate(workflow.SubDoc(), "t", script); e == nil || !strings.Contains(e.Error(), "missing script result for sub-workflow") {
		t.Fatal("unscripted child result", e)
	}
	script["consult"] = []workflow.ScriptStep{{Evidence: []workflow.Evidence{{ID: "c-1", TaskID: "t", Stage: "consult", Type: "consult", Version: 1, Provenance: true, Fields: map[string]any{"outputs": map[string]any{"note": map[string]any{"text": "ok"}}}}}}}
	s, e := simulate(workflow.SubDoc(), "t", script)
	if e != nil || s.State != domain.Done || !strings.Contains(strings.Join(s.Path, ","), "consult,review") {
		t.Fatal(s.Path, e)
	}
	script["consult"] = []workflow.ScriptStep{{Next: "stop"}}
	if s, e = simulate(workflow.SubDoc(), "t", script); e != nil || s.State != domain.Escalated || s.Path[len(s.Path)-2] != "stop" {
		t.Fatal("child failure route", s.Path, e)
	}
	script["consult"] = []workflow.ScriptStep{{Evidence: []workflow.Evidence{{ID: "c-1", TaskID: "t", Stage: "consult", Type: "consult", Version: 1, Provenance: true, Fields: map[string]any{}}}}}
	if _, e = simulate(workflow.SubDoc(), "t", script); e == nil {
		t.Fatal("schema-invalid child result accepted")
	}
}

// A Local simulation pins the Local projection; a Normal one runs the
// Normal-only tail with scripted (never performed) results.
func TestSimulateNormalTail(t *testing.T) {
	d := workflow.NormalTailDoc()
	script := workflow.Script{"change": record("change", map[string]any{}), "test": record("test", greenRun), "review": record("review", map[string]any{"approved": true}), "commit": record("commit", map[string]any{"sha": "simulated"})}
	s, err := simulate(d, "t", script)
	if err != nil || s.State != domain.Done || strings.Join(s.Path, ",") != "change,test,gate,review,commit,Done" {
		t.Fatal("Local simulation reached a Normal-only stage", s, err)
	}
	receipt := func(stage string) []workflow.ScriptStep {
		return []workflow.ScriptStep{{Evidence: []workflow.Evidence{{ID: stage + "-1", TaskID: "t", Stage: stage, Type: "receipt", Version: 1, Provenance: true, Fields: map[string]any{}}}}}
	}
	script["create"], script["read"] = receipt("create"), receipt("read")
	s, err = engine.Simulate(d, "t", script, workflow.SimulationOptions{Normal: true})
	if err != nil || s.State != domain.Done || strings.Join(s.Path, ",") != "change,test,gate,review,commit,create,read,Done" {
		t.Fatal("Normal simulation skipped its sub-workflow", s, err)
	}
}

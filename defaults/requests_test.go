package defaults

import (
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func report(request map[string]any) engine.ScriptStep {
	r := map[string]any{"changed_files": []string{"answer.go"}, "hunks": []string{"return 42"}, "proof": "scripted fixture", "source": redSource, "test_id": "TestBehavior"}
	if request != nil {
		r["request"] = request
	}
	return engine.ScriptStep{Output: raw(r)}
}
func decision(approved bool) engine.ScriptStep {
	return engine.ScriptStep{Output: raw(map[string]any{"approved": approved, "rationale": "fixture"})}
}

var widenRequest = map[string]any{"kind": workflow.RequestScopeWiden, "files": []string{"pkg/helper.go"}, "reason": "the fix touches the helper"}
var reworkRequest = map[string]any{"kind": workflow.RequestRedRework, "reason": "the RED asserts the wrong value"}

// FAE-WF-078 in the shipped default: the implementer's request is judged by
// the independent oracle, counted against its cap, and an approved widen is
// carried into every later dispatch; the briefs say so (FAE-WF-083).
func TestDefaultGovernedRequests(t *testing.T) {
	d := Workflow()
	p := d.Requests
	if p == nil || !slices.Equal(p.From, []string{"implement"}) || p.Judge != "request-judge" || p.Widen != "implement-loop" || p.Rework != "red-loop" || p.ReworkCap != 3 || d.Limits.ScopeWidens != 3 {
		t.Fatalf("default request policy %+v limits %+v", p, d.Limits)
	}
	if judge := d.Stages[p.Judge]; judge.Role != "oracle" || judge.Role == d.Stages["implement"].Role {
		t.Fatal("request judge is not the independent oracle", judge.Role)
	}
	brief := d.Roles["coder"].Briefs["implement"]
	for _, clause := range []string{"request a RED rework", "request a scope widen", "independent judge"} {
		if !strings.Contains(brief, clause) {
			t.Errorf("implement brief lacks %q", clause)
		}
	}
	if strings.Contains(brief, "in the proof note") {
		t.Error("implement brief still routes requests through the proof note")
	}

	script := HappyScript()
	script["implement"] = []engine.ScriptStep{report(widenRequest), report(nil)}
	script["request-judge"] = []engine.ScriptStep{decision(true)}
	j, dispatches := driveDispatches(t, Workflow(), script)
	if j.State != domain.Done || !slices.Equal(j.ApprovedScope, []string{"pkg/helper.go"}) || j.RequestCounts[workflow.RequestScopeWiden] != 1 {
		t.Fatal("approved widen", j.State, j.Reason, j.ApprovedScope, j.RequestCounts)
	}
	carried := false
	for _, x := range dispatches {
		if x.Stage == "implement" && x.Visit == 2 {
			carried = slices.Equal(x.Scope, []string{"pkg/helper.go"})
		}
	}
	if !carried || !routed(j, "implement", "request-judge") || !routed(j, "request-judge", "implement-loop") {
		t.Fatal("approved scope not carried into the next implementation")
	}

	script = HappyScript()
	script["implement"] = []engine.ScriptStep{report(widenRequest)}
	script["request-judge"] = []engine.ScriptStep{decision(false)}
	if j = drive(t, script); j.EscalationKind != domain.ScopeWidenRejected {
		t.Fatal("rejected widen", j.EscalationKind, j.Reason)
	}
	script = HappyScript()
	script["implement"] = []engine.ScriptStep{report(widenRequest)}
	script["request-judge"] = []engine.ScriptStep{decision(true)}
	if j = drive(t, script); j.EscalationKind != domain.ScopeWidenRejected || !strings.Contains(j.Reason, "cap 3 exhausted") {
		t.Fatal("widen cap", j.EscalationKind, j.Reason)
	}

	script = HappyScript()
	script["implement"] = []engine.ScriptStep{report(reworkRequest), report(nil)}
	script["request-judge"] = []engine.ScriptStep{decision(true)}
	j, dispatches = driveDispatches(t, Workflow(), script)
	if j.State != domain.Done || !routed(j, "request-judge", "red-loop") || j.RequestCounts[workflow.RequestRedRework] != 1 {
		t.Fatal("approved RED rework", j.State, j.Reason)
	}
	bound := false
	for _, x := range dispatches {
		if x.Stage == "test-first" && x.Visit == 2 {
			for _, a := range x.Inputs {
				bound = bound || a.Stage == "request-judge"
			}
		}
	}
	if !bound {
		t.Fatal("RED rework turn lacks the approving decision")
	}
}

// FAE-WF-073 in the shipped default: only a spec's no-red declaration skips
// the RED sequence, and the change is then proven by the whole suite.
func TestDefaultNoRedRoute(t *testing.T) {
	if d := Workflow(); !slices.Equal(d.NoRedStages, []string{"spec"}) {
		t.Fatal("default declares no no-red stage", d.NoRedStages)
	}
	script := HappyScript()
	script["spec"] = []engine.ScriptStep{{Output: raw(map[string]any{"scope": "docs", "acceptance": []string{"documented"}, "contracts": []string{"none"}, "risks": []string{"none"}, "required_tests": []string{"none"}, "declared_files": []string{"README.md"}, "no_red": true})}}
	script["implement"] = []engine.ScriptStep{{Output: raw(map[string]any{"changed_files": []string{"README.md"}, "hunks": []string{"docs"}, "proof": "documentation only", "source": "", "test_id": ""})}}
	script["run-suite"] = []engine.ScriptStep{{Output: raw(map[string]any{"exit_code": 0, "executed_count": 3, "matched_count": 0, "expected_failure": false, "test_in_source": false, "red_passed": false, "fresh_build": true, "output_digest": "suite"})}}
	j, dispatches := driveDispatches(t, Workflow(), script)
	if j.State != domain.Done || !routed(j, "red-route", "implement-loop") || !routed(j, "green-route", "run-suite") {
		t.Fatal("no-red route", j.State, j.Reason)
	}
	for _, x := range dispatches {
		if x.Stage == "test-first" || x.Stage == "run-red" || x.Stage == "freeze-red" || x.Stage == "compare-red" || x.Stage == "run-tests" {
			t.Fatal("no-red task ran the RED sequence at", x.Stage)
		}
	}
}

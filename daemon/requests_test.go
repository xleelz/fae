package daemon

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-078 acceptance: approve a widen, restart the daemon, and the widened
// set is in the next dispatch's brief scope with the cap counter unchanged.
func TestDaemonApprovedWidenSurvivesRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	route := []workflow.Binding{{Provider: "script", Model: "recorded"}}
	doc := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "requests", Start: "work", Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 3},
		Roles: map[string]workflow.Role{
			"coder": {Instructions: "code", Briefs: map[string]string{"default": "brief"}, Routes: route, Scope: "task-bound", Cap: 1, Outputs: []string{"Report"}},
			"judge": {Instructions: "judge", Briefs: map[string]string{"default": "brief"}, Routes: route, Scope: "task-bound", Cap: 1, Outputs: []string{"Decision"}},
		},
		Artifacts: map[string]workflow.ArtifactType{"Report": {Version: 1, Schema: json.RawMessage(`{"type":"object","properties":{"request":{"type":"object"}}}`)}, "Decision": {Version: 1, Schema: json.RawMessage(`{"type":"object","properties":{"approved":{"type":"boolean"}},"required":["approved"]}`)}},
		Stages: map[string]workflow.Stage{
			"work":       {Primitive: workflow.AgentTurn, Role: "coder", Output: "Report"},
			"judge":      {Primitive: workflow.AgentTurn, Role: "judge", Output: "Decision", Inputs: []workflow.Input{{Name: "report", Type: "Report", Stage: "work", Version: 1}}},
			"widen-loop": {Primitive: workflow.Loop, MaxIterations: 3},
		},
		Edges:    []workflow.Edge{{From: "work", To: "Done"}, {From: "work", To: "judge"}, {From: "judge", To: "widen-loop"}, {From: "widen-loop", To: "work"}},
		Requests: &workflow.RequestPolicy{From: []string{"work"}, Judge: "judge", Widen: "widen-loop"},
	})
	script := map[string][]engine.ScriptStep{
		"work":  {{Output: json.RawMessage(`{"request":{"kind":"scope-widen","files":["pkg/extra.go"],"reason":"helper must change"}}`)}, {Output: json.RawMessage(`{}`), Delay: 2 * time.Second}},
		"judge": {{Output: json.RawMessage(`{"approved":true}`)}},
	}
	cfg.Executor = engine.NewScripted(script)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "widen", Tree: "tree", WorkflowID: "requests", AuthorizingMessage: "message"}, "widen"); err != nil {
		t.Fatal(err)
	}
	var token string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && token == ""; time.Sleep(10 * time.Millisecond) {
		if s, err := d.store.GetJob(ctx, "widen"); err == nil {
			var e envelope
			_ = json.Unmarshal(s.Payload, &e)
			if f := e.Job.InFlight; f != nil && f.Stage == "work" && f.Visit == 2 {
				if !slices.Equal(f.Scope, []string{"pkg/extra.go"}) || e.Job.RequestCounts["scope-widen"] != 1 {
					stop(t, d)
					t.Fatal("approved widen not in the next dispatch", f.Scope, e.Job.RequestCounts)
				}
				token = f.ID
			}
		}
	}
	stop(t, d)
	if token == "" {
		t.Fatal("no post-approval dispatch")
	}
	cfg.Executor = engine.NewScripted(script)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	e := liveEnvelope(t, d, "widen")
	if f := e.Job.InFlight; f == nil || f.ID != token || !slices.Equal(f.Scope, []string{"pkg/extra.go"}) {
		t.Fatal("restart dropped the approved scope", f)
	}
	awaitState(t, d, "widen", "Done")
	e = liveEnvelope(t, d, "widen")
	if !slices.Equal(e.Job.ApprovedScope, []string{"pkg/extra.go"}) || e.Job.RequestCounts["scope-widen"] != 1 || e.Job.Counts["judge"] != 1 {
		t.Fatal("restart changed request state", e.Job.ApprovedScope, e.Job.RequestCounts, e.Job.Counts)
	}
}

// FAE-WF-086: the operator's carry-forward re-admission of an escalated item
// with unchanged acceptance text skips the accepted pre-code stages. It is
// admitted Pending and released like any other task, entering the carried
// entry stage at release (FAE-VCS-001/-008).
func TestDaemonCarryForwardReadmission(t *testing.T) {
	cfg, ctx := fixture(t)
	script := defaults.HappyScript()
	script["first/test-first"] = []engine.ScriptStep{{Class: "execution-error"}, {Class: "execution-error"}, {Class: "execution-error"}, {Class: "execution-error"}}
	cfg.Executor = engine.NewScripted(script)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "first", Tree: "tree", Content: "make answer 42", AuthorizingMessage: "message"}, "first"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "first", "Escalated")
	if _, err = call(t, d, "Admit", AdmitParams{ID: "changed", Tree: "tree", Content: "make answer 43", CarryFrom: "first", AuthorizingMessage: "message"}, "changed"); err == nil {
		t.Fatal("changed acceptance text carried forward")
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "second", Tree: "tree", Content: "make answer 42", CarryFrom: "first", AuthorizingMessage: "message"}, "second"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "second", "Done")
	e := liveEnvelope(t, d, "second")
	carried := 0
	for _, p := range e.Job.Provenance {
		if p.CarriedFrom == "first" {
			carried++
		}
	}
	if carried < 4 || e.Job.Counts["spec"] != 0 || e.Job.Counts["plan"] != 0 || e.Job.Counts["test-first"] != 1 {
		t.Fatal("carry-forward", carried, e.Job.Counts)
	}
	admitted, released := false, false
	for _, h := range e.Job.History {
		admitted = admitted || h.Kind == "carry-forward" && h.Stage == "Pending" && strings.Contains(h.Cause, "carried from first")
		released = released || h.Kind == "transition" && h.From == "Pending" && h.To == "red-loop" && admitted
	}
	if !admitted || !released || e.Job.CarryEntry != "" {
		t.Fatal("carry-forward not admitted Pending and released at its entry", admitted, released, e.Job.CarryEntry)
	}
}

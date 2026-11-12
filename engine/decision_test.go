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

func decisionDoc() workflow.Document {
	d := floorDoc()
	d.Artifacts["Decision"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","required":["decision_id","option","authorizing_message","decided_by"],"properties":{"option":{"enum":["ship","rework","halt"]}}}`)}
	d.Stages["approve"] = workflow.Stage{Primitive: workflow.HumanDecision, Question: "Ship the reviewed change?", Options: []workflow.DecisionOption{{ID: "ship", Next: "commit"}, {ID: "rework", Next: "again"}, {ID: "halt", Next: "stop"}}, OnTimeout: workflow.StayWaiting, Output: "Decision", Inputs: []workflow.Input{{Name: "review", Type: "report", Stage: "review", Version: 1}}}
	d.Stages["again"] = workflow.Stage{Primitive: workflow.Loop, MaxIterations: 2}
	replaceEdge(&d, workflow.Edge{From: "review", To: "commit"}, workflow.Edge{From: "review", To: "approve"}, workflow.Edge{From: "approve", To: "commit"}, workflow.Edge{From: "approve", To: "again"}, workflow.Edge{From: "approve", To: "stop"}, workflow.Edge{From: "again", To: "change"})
	return d
}
func decisionJob(t *testing.T) engine.Job {
	t.Helper()
	j, e := engine.New("task", 1, decisionDoc(), "tree")
	if e != nil {
		t.Fatal(e)
	}
	return j
}

// FAE-WF-030 HumanDecision conformance on the scripted adapter: park and raise
// HumanDecisionRequired, stay waiting, route each declared option, record a
// decision artifact citing the message, refuse a second decision.
func TestHumanDecisionParksDecidesAndRoutes(t *testing.T) {
	x := engine.NewScripted(floorScript(2))
	j := drive(t, decisionJob(t), x, true)
	if j.State != "approve" || !j.Parked || j.Reason != engine.ParkHumanDecision {
		t.Fatal("not waiting", j.State, j.Reason)
	}
	last := j.History[len(j.History)-1]
	if last.Kind != "park" || last.EscalationKind != domain.HumanDecisionRequired || !strings.HasPrefix(last.DispatchID, "decision-") {
		t.Fatal("HumanDecisionRequired not raised", last)
	}
	req, ok := engine.PendingDecision(j)
	if !ok || req.Visit != 1 || req.Question != "Ship the reviewed change?" || len(req.Options) != 3 || req.ID != last.DispatchID {
		t.Fatal(req, ok)
	}
	attempts := j.Attempts
	// Stay waiting: repeated rounds (and a restart) never decide or dispatch.
	for i := 0; i < 3; i++ {
		next, d, events, e := engine.Prepare(roundTrip(t, j))
		if e != nil || d != nil || len(events) != 0 || next.State != "approve" || !next.Parked {
			t.Fatal("a waiting decision moved", d, events, e)
		}
	}
	for name, bad := range map[string]engine.Decision{"no message": {Option: "ship"}, "unknown option": {Option: "merge", Message: "m"}, "rationale bound": {Option: "ship", Message: "m", Rationale: strings.Repeat("r", 4097)}} {
		if out, _, _, e := engine.Decide(j, bad); e == nil || out.State != "approve" {
			t.Fatal(name, "accepted")
		}
	}
	j, events, rec, e := engine.Decide(roundTrip(t, j), engine.Decision{Option: "rework", Message: "m1", Actor: "operator", Rationale: "tighten it"})
	if e != nil || j.State != "again" || j.Parked || rec.ID != req.ID || rec.Option != "rework" || rec.Next != "again" || rec.Message != "m1" || rec.ArtifactID == "" || rec.Revision != 1 {
		t.Fatal(rec, j.State, e)
	}
	if events[0].Kind != "decision" || events[len(events)-1].To != "again" || j.Attempts != attempts {
		t.Fatal("decision events or attempt charge", events, j.Attempts)
	}
	if _, _, _, e = engine.Decide(j, engine.Decision{Option: "ship", Message: "m1"}); e == nil {
		t.Fatal("second decision for the same visit accepted")
	}
	j = drive(t, j, x, true)
	second, ok := engine.PendingDecision(j)
	if !ok || second.Visit != 2 || second.ID == req.ID {
		t.Fatal("revisit must raise a new decision", second, ok)
	}
	j, _, rec, e = engine.Decide(j, engine.Decision{Option: "ship", Message: "m2"})
	if e != nil || rec.Visit != 2 {
		t.Fatal(e)
	}
	j = drive(t, j, x, true)
	if j.State != domain.Done {
		t.Fatal(j.State, j.Reason)
	}
	decisions := 0
	for _, a := range j.Evidence {
		if a.Stage != "approve" {
			continue
		}
		decisions++
		if a.Role != "operator" || !strings.HasPrefix(a.Session, "decision-") || a.Fields["question"] != "Ship the reviewed change?" || a.Fields["authorizing_message"] == "" {
			t.Fatal(a)
		}
		for _, p := range j.Provenance {
			if p.ArtifactID == a.ID && (p.Primitive != workflow.HumanDecision || p.DispatchID != a.Session || len(p.Inputs) != 1) {
				t.Fatal("decision provenance", p)
			}
		}
	}
	if decisions != 2 || engine.Validate(j) != nil {
		t.Fatal("decision artifacts", decisions)
	}
	if _, _, _, e = engine.Decide(j, engine.Decision{Option: "ship", Message: "m3"}); e == nil {
		t.Fatal("terminal task decided")
	}
}

func TestHumanDecisionHaltAndCancellation(t *testing.T) {
	x := engine.NewScripted(floorScript(2))
	waiting := drive(t, decisionJob(t), x, false)
	j, _, _, e := engine.Decide(waiting, engine.Decision{Option: "halt", Message: "m"})
	if e == nil {
		j = drive(t, j, x, false)
	}
	if e != nil || j.State != domain.Escalated || j.EscalationKind != domain.HumanDecisionRequired {
		t.Fatal("halt option", j.State, j.EscalationKind, e)
	}
	now := time.Now().UTC()
	blocked, _, e := engine.ApplyControl(waiting, engine.Control{Method: "Block", Reason: "hold"}, now)
	if e != nil || blocked.State != domain.Blocked {
		t.Fatal(e)
	}
	if _, ok := engine.PendingDecision(blocked); ok {
		t.Fatal("blocked task still pending")
	}
	if _, _, _, e = engine.Decide(blocked, engine.Decision{Option: "ship", Message: "m"}); e == nil {
		t.Fatal("blocked task decided")
	}
	cancelled, _, e := engine.ApplyControl(blocked, engine.Control{Method: "Cancel", Reason: "not needed"}, now)
	if e != nil || cancelled.State != domain.Cancelled {
		t.Fatal(e)
	}
	if _, _, _, e = engine.Decide(cancelled, engine.Decision{Option: "ship", Message: "m"}); e == nil {
		t.Fatal("cancelled task decided")
	}
	escalated, _, e := engine.ApplyControl(waiting, engine.Control{Method: "Escalate", Reason: "ask later"}, now)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, _, e = engine.Decide(escalated, engine.Decision{Option: "ship", Message: "m"}); e == nil {
		t.Fatal("escalated task decided")
	}
	restarted, _, e := engine.ApplyControl(escalated, engine.Control{Method: "RestartWithInformation", Information: "decide again"}, now)
	if e != nil {
		t.Fatal(e)
	}
	again := drive(t, restarted, x, false)
	if req, ok := engine.PendingDecision(again); !ok || req.Visit != 2 {
		t.Fatal("restarted task must wait for a fresh decision", req, ok)
	}
}

// Kernel floors around a decision: no-bypass inputs, schema-validated
// decision artifacts, and migration voiding a pending decision.
func TestHumanDecisionFloors(t *testing.T) {
	x := engine.NewScripted(floorScript(1))
	d := decisionDoc()
	s := d.Stages["approve"]
	s.Inputs = append(s.Inputs, workflow.Input{Name: "missing", Type: "Decision", Stage: "approve", Version: 1})
	d.Stages["approve"] = s
	j, e := engine.New("task", 1, d, "tree")
	if e != nil {
		t.Fatal(e)
	}
	j = drive(t, j, x, false)
	if _, ok := engine.PendingDecision(j); ok || !strings.HasPrefix(j.Reason, "MissingInput") {
		t.Fatal("a decision bypassed a missing input", j.Reason)
	}
	d = decisionDoc()
	d.Artifacts["Decision"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","required":["signed_off"]}`)}
	if j, e = engine.New("task", 1, d, "tree"); e != nil {
		t.Fatal(e)
	}
	j = drive(t, j, x, false)
	if out, _, _, e := engine.Decide(j, engine.Decision{Option: "ship", Message: "m"}); e == nil || !strings.Contains(e.Error(), "does not satisfy Decision") || out.State != "approve" || len(out.Evidence) != len(j.Evidence) {
		t.Fatal("schema-invalid decision artifact recorded", e)
	}
	next := decisionDoc()
	next.Stages["approve"] = workflow.Stage{Primitive: workflow.HumanDecision, Question: "Ship it now?", Options: d.Stages["approve"].Options, OnTimeout: workflow.StayWaiting, Output: "Decision"}
	moved, _, e := engine.Migrate(drive(t, decisionJob(t), x, false), next, 2, "")
	if e != nil || moved.Parked {
		t.Fatal("migration must void the pending decision", e)
	}
	moved, _, _, e = engine.Prepare(moved)
	if req, ok := engine.PendingDecision(moved); e != nil || !ok || req.Question != "Ship it now?" || req.Visit != 2 {
		t.Fatal(req, ok, e)
	}
}

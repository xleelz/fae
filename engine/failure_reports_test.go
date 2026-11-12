package engine_test

import (
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
)

func TestFailureReportsUseRecordedReasonAndCiteGate(t *testing.T) {
	passed := false
	j := engine.Job{Document: workflow.Document{Stages: map[string]workflow.Stage{"primary": {Primitive: workflow.Gate, Failure: "repair"}, "repair": {Primitive: workflow.AgentTurn}}}, History: []engine.Event{
		{Kind: "gate", Stage: "primary", BranchID: "outer/one", Gate: &workflow.GateResult{Passed: false}, Record: "gate-record"},
		{Kind: "renamed-event", Failure: &engine.FailureRoute{Stage: "primary", Visit: 2, Branch: "outer/one", Reason: "binding", Detail: "missing component", Handler: "repair"}},
		{Kind: "transition", Stage: "primary", BranchID: "outer/one", From: "primary", To: "repair"},
		{Kind: "transition", Stage: "repair", BranchID: "outer/one", From: "repair", To: domain.Done},
		{Kind: "anything", Failure: &engine.FailureRoute{Stage: "call", Visit: 3, Reason: "recursion_budget", Detail: "max depth"}},
	}}
	routes, refusals := engine.FailureReports(j)
	if len(routes) != 1 || len(refusals) != 2 {
		t.Fatal(routes, refusals)
	}
	r := routes[0]
	if r.HandlerPassed == nil || *r.HandlerPassed == passed || len(r.Records) != 1 || r.Records[0] != "gate-record" || r.Visit != 2 || r.Branch != "outer/one" {
		t.Fatal(r)
	}
	if !refusals[1].Escalated {
		t.Fatal(refusals)
	}
}

package engine_test

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
)

func TestStageBudgetResumeOncePrecedesFailureRoute(t *testing.T) {
	doc := failureDoc()
	r := doc.Roles["author"]
	r.TurnBudget = &workflow.TurnBudgetSpec{WallSeconds: 3600, MaxAssistantTurns: 300, OnExhaustion: "fail"}
	doc.Roles["author"] = r
	s := doc.Stages["primary"]
	s.TurnBudget = &workflow.TurnBudgetSpec{WallSeconds: 900, MaxAssistantTurns: 200, OnExhaustion: "resume-once"}
	doc.Stages["primary"] = s
	j := newPrimitiveJob(t, doc)
	x := engine.NewScripted(map[string][]engine.ScriptStep{"primary": {{Class: engine.ClassTurnBudgetExhausted}}, "repair": step(`{"text":"repaired"}`)})
	for n := 0; n < 2; n++ {
		var d *engine.Dispatch
		var err error
		for d == nil {
			j, d, _, err = engine.Prepare(j)
			if err != nil {
				t.Fatal(err)
			}
		}
		result, err := x.Execute(context.Background(), *d)
		if err != nil {
			t.Fatal(err)
		}
		j, _, err = engine.Complete(j, d.ID, result)
		if err != nil {
			t.Fatal(err)
		}
		j = roundTrip(t, j)
		if n == 0 && j.State != "primary" {
			t.Fatal("resume-once bypassed", j.State, j.Reason)
		}
	}
	if j.State != "repair" {
		t.Fatal(j.State, j.Reason)
	}
	var budget bool
	for _, a := range j.Evidence {
		budget = budget || a.Type == "failure" && a.Fields["reason"] == "budget"
	}
	if !budget {
		t.Fatal("missing budget failure evidence")
	}
	j = drive(t, j, x, true)
	if j.State != domain.Done {
		t.Fatal(j.State, j.Reason)
	}
	s.TurnBudget.WallSeconds = 3601
	doc.Stages["primary"] = s
	if issues := workflow.Validate(doc); len(issues) == 0 {
		t.Fatal("looser stage budget accepted")
	}
}

func TestBudgetWithoutRouteFailsOnceInsteadOfSameGateRetry(t *testing.T) {
	doc := dataWorkflow()
	doc.Signature = nil
	doc.Start = "make"
	doc.Stages = map[string]workflow.Stage{"make": doc.Stages["make"]}
	doc.Edges = []workflow.Edge{{From: "make", To: "Done"}}
	j := drive(t, newPrimitiveJob(t, doc), engine.NewScripted(map[string][]engine.ScriptStep{"make": {{Class: engine.ClassTurnBudgetExhausted, Output: json.RawMessage(`{}`)}}}), true)
	if j.State != domain.Escalated || j.Counts["make"] != 1 {
		t.Fatal(j.State, j.Counts, j.Reason)
	}
}

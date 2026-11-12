package engine_test

import (
	"encoding/json"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
)

func TestScenarioRecursionCycleRequiresEveryIntermediateBudget(t *testing.T) {
	for _, budgetB := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-intermediate", true: "fully-budgeted"}[budgetB], func(t *testing.T) {
			docs := map[string]workflow.Document{}
			for _, id := range []string{"A", "B", "C", "D"} {
				d := scenarioLeafDoc()
				d.ID = id
				if id != "B" || budgetB {
					d.Recursion = &workflow.Recursion{MaxDepth: 8, MaxDescendants: 10, MaxChildrenPerVisit: 2}
				}
				docs[id] = d
			}
			ids := []string{"root", "root.detached.1", "root.detached.1.detached.1", "root.detached.1.detached.1.detached.1"}
			rules := []workflow.ScenarioRule{}
			for i, next := range []string{"B", "C", "D", "A"} {
				rules = append(rules, workflow.ScenarioRule{Stage: "make", TaskGlob: ids[i], Output: json.RawMessage(`{"text":"ok"}`), Detached: []workflow.ScenarioAdmission{{Workflow: next}}})
			}
			report, err := engine.SimulateScenario(docs["A"], "root", workflow.Scenario{Seed: 7, Workflows: docs, Rules: rules}, workflow.SimulationOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if budgetB {
				if len(report.Tree) != 5 || len(report.Refusals) != 0 {
					t.Fatal(report)
				}
			} else {
				if len(report.Tree) != 4 || len(report.Refusals) != 1 || !strings.Contains(report.Refusals[0].Reason, "cycle requires budgets") {
					t.Fatal(report)
				}
			}
		})
	}
}

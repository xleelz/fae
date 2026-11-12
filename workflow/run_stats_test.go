package workflow_test

import (
	"encoding/json"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
)

func TestRunStatsEngineProducerCannotBeDeclaredByContent(t *testing.T) {
	for _, kind := range []string{"stage", "artifact"} {
		d := defaults.Workflow()
		if kind == "stage" {
			d.Stages["engine:run_stats"] = d.Stages[d.Start]
		} else {
			d.Artifacts["engine:run_stats"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
		}
		found := false
		for _, issue := range workflow.Validate(d) {
			if strings.Contains(issue.Message, "reserved engine-provided") {
				found = true
			}
		}
		if !found {
			t.Fatal("content can impersonate engine stats producer", kind)
		}
	}
}
func TestRunStatsSyntheticProducerAndTypeResolve(t *testing.T) {
	d := defaults.Workflow()
	if _, ok := workflow.ArtifactDeclaration(d, "engine:run_stats"); !ok {
		t.Fatal("missing engine stats type")
	}
	if _, ok := workflow.ProducerDeclaration(d, "engine:run_stats"); !ok {
		t.Fatal("missing engine stats producer")
	}
}

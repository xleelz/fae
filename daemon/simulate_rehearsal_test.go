package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// FAE-WF-091: a scripted-adapter scenario given to the simulator (the
// WorkflowSimulate RPC as well as fae workflow simulate) without a task takes
// the one task its task-scoped keys name, as a record scenario takes the one
// its evidence names; one naming no single task is refused with the remedy.
// Before, any scripted-adapter scenario was refused as an unknown field.
func TestRehearsalScenarioInfersItsTaskFromScopedKeys(t *testing.T) {
	scoped := map[string][]engine.ScriptStep{}
	for stage, steps := range defaults.HappyScript() {
		scoped["demo/"+stage] = steps
	}
	raw, err := json.Marshal(scoped)
	if err != nil {
		t.Fatal(err)
	}
	s, err := SimulateWorkflow(defaults.Workflow(), "", raw)
	if err != nil || s.State != domain.Done {
		t.Fatalf("task-scoped scenario without a task: %v %s %s", err, s.State, s.Reason)
	}
	if raw, err = json.Marshal(defaults.HappyScript()); err != nil {
		t.Fatal(err)
	}
	if _, err = SimulateWorkflow(defaults.Workflow(), "", raw); err == nil || !strings.Contains(err.Error(), "select one with --task") {
		t.Fatalf("a scenario naming no task was not refused with the remedy: %v", err)
	}
	if s, err = SimulateWorkflow(defaults.Workflow(), "any", raw); err != nil || s.State != domain.Done {
		t.Fatalf("plain keys apply to every task: %v %s", err, s.State)
	}
}

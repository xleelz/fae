package daemon

import (
	agentruntime "github.com/xleelz/fae/runtime"
	"testing"
)

func TestResidentPressureClassification(t *testing.T) {
	for _, tc := range []struct {
		role, policy           string
		global, pre, protected bool
	}{
		{"spec", "workflow-global", true, true, false},
		{"planner", "workflow-global", true, true, false},
		{"spec", "task-bound", false, false, false},
		{"custom", "workflow-global", true, false, false},
		{"oracle", "workflow-global", true, false, true},
		{"qa", "task-bound", false, false, true},
	} {
		g, p, k := classifyResident(agentruntime.Launch{Role: tc.role, SessionPolicy: tc.policy})
		if g != tc.global || p != tc.pre || k != tc.protected {
			t.Errorf("%+v: got %v %v %v", tc, g, p, k)
		}
	}
}

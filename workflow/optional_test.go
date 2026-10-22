package workflow

import "testing"

func TestOptionalTypedInputDoesNotWeakenRequiredBindings(t *testing.T) {
	stage := Stage{Inputs: []Input{{Name: "required", Type: "FrozenIdentity", Stage: "freeze", Version: 1}, {Name: "conflict", Type: "ReconcileReport", Stage: "merge", Version: 1, Optional: true}}}
	evidence := []Evidence{{ID: "id", TaskID: "task", Type: "FrozenIdentity", Stage: "freeze", Version: 1, Provenance: true}}
	if got := CheckInputs(stage, "task", evidence); len(got) != 0 {
		t.Fatal(got)
	}
	evidence[0].TaskID = "other"
	if got := CheckInputs(stage, "task", evidence); len(got) != 1 || got[0] != "required" {
		t.Fatal("optional weakened required identity", got)
	}
}

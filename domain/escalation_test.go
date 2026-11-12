package domain

import "testing"

func TestEscalationKindsAreTheClosedAppendixBSet(t *testing.T) {
	want := []string{"AttemptCap", "LoopCap", "SameGate", "TestRunCap", "ScopeWidenRejected", "NoRedViolation", "CoordinatorVeto", "StaleBranchResidue", "DispatchRecoveryExhausted", "StallExhausted", "ProviderUnavailable", "PaneCapStarvation", "ResumeEvidence", "MergeOwnHunkLoss", "IntegrationConflict", "ExternalEffectRefused", "ReviewGateFailed", "DryRunComplete", "OperatorEscalate", "HumanDecisionRequired", "FootprintExceedsHost"}
	if len(EscalationKinds) != len(want) {
		t.Fatal(EscalationKinds)
	}
	for i, k := range EscalationKinds {
		if string(k) != want[i] || !k.Valid() {
			t.Fatal(i, k)
		}
	}
	for _, k := range []EscalationKind{"", "DispatchRecoveryCap", "StartupExhausted", "UncertainDispatch", "MalformedOutput", "attemptcap"} {
		if k.Valid() {
			t.Fatal("open kind accepted", k)
		}
	}
	if e := (Escalation{Kind: LoopCap, Detail: "spec-loop"}); e.String() != "LoopCap: spec-loop" || (Escalation{Kind: AttemptCap}).String() != "AttemptCap" {
		t.Fatal(e.String())
	}
}

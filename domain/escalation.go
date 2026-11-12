package domain

// EscalationKind is the closed Appendix B set. Every per-task escalation
// carries exactly one kind; the specific cause belongs in Escalation.Detail.
type EscalationKind string

const (
	AttemptCap                EscalationKind = "AttemptCap"
	LoopCap                   EscalationKind = "LoopCap"
	SameGate                  EscalationKind = "SameGate"
	TestRunCap                EscalationKind = "TestRunCap"
	ScopeWidenRejected        EscalationKind = "ScopeWidenRejected"
	NoRedViolation            EscalationKind = "NoRedViolation"
	CoordinatorVeto           EscalationKind = "CoordinatorVeto"
	StaleBranchResidue        EscalationKind = "StaleBranchResidue"
	DispatchRecoveryExhausted EscalationKind = "DispatchRecoveryExhausted"
	StallExhausted            EscalationKind = "StallExhausted"
	ProviderUnavailable       EscalationKind = "ProviderUnavailable"
	PaneCapStarvation         EscalationKind = "PaneCapStarvation"
	ResumeEvidence            EscalationKind = "ResumeEvidence"
	MergeOwnHunkLoss          EscalationKind = "MergeOwnHunkLoss"
	IntegrationConflict       EscalationKind = "IntegrationConflict"
	ExternalEffectRefused     EscalationKind = "ExternalEffectRefused"
	ReviewGateFailed          EscalationKind = "ReviewGateFailed"
	DryRunComplete            EscalationKind = "DryRunComplete"
	OperatorEscalate          EscalationKind = "OperatorEscalate"
	HumanDecisionRequired     EscalationKind = "HumanDecisionRequired"
	FootprintExceedsHost      EscalationKind = "FootprintExceedsHost"
)

// EscalationKinds lists the set in Appendix B order.
var EscalationKinds = []EscalationKind{AttemptCap, LoopCap, SameGate, TestRunCap, ScopeWidenRejected, NoRedViolation, CoordinatorVeto, StaleBranchResidue, DispatchRecoveryExhausted, StallExhausted, ProviderUnavailable, PaneCapStarvation, ResumeEvidence, MergeOwnHunkLoss, IntegrationConflict, ExternalEffectRefused, ReviewGateFailed, DryRunComplete, OperatorEscalate, HumanDecisionRequired, FootprintExceedsHost}

func (k EscalationKind) Valid() bool {
	for _, known := range EscalationKinds {
		if k == known {
			return true
		}
	}
	return false
}

type Escalation struct {
	Kind   EscalationKind `json:"kind"`
	Detail string         `json:"detail,omitempty"`
}

// String is the operator-facing reason: the kind, then any detail.
func (e Escalation) String() string {
	if e.Detail == "" {
		return string(e.Kind)
	}
	return string(e.Kind) + ": " + e.Detail
}

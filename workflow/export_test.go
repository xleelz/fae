package workflow

// Test documents shared with the external simulator tests, which run the
// engine (engine.Simulate) and so cannot live in this package.
var (
	MinimalDoc    = minimal
	ProtectedDoc  = protected
	DecisionDoc   = decisionDoc
	ConfigDoc     = configDoc
	FanDoc        = fanDoc
	SubDoc        = subDoc
	NormalTailDoc = normalTail
)

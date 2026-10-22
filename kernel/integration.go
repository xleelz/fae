package kernel

// CanRetryIntegration permits a new reconciliation only after a known no-write
// refusal, an observed moved target, a known own commit and a bounded route.
// @ ensures allowed == (noWrite && tipAdvanced && ownCommitKnown && boundedRoute)
// @ ensures allowed ==> noWrite && tipAdvanced && ownCommitKnown && boundedRoute
func CanRetryIntegration(noWrite, tipAdvanced, ownCommitKnown, boundedRoute bool) (allowed bool) {
	return noWrite && tipAdvanced && ownCommitKnown && boundedRoute
}

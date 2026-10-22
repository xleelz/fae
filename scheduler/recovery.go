package scheduler

// CanRecoverLease excludes expiry and partial process observations.
// @ ensures allowed == (ownerDead && linkageMatches && (neverLaunched || retired))
// @ ensures allowed ==> ownerDead && linkageMatches && (neverLaunched || retired)
func CanRecoverLease(ownerDead, linkageMatches, neverLaunched, retired bool) (allowed bool) {
	return ownerDead && linkageMatches && (neverLaunched || retired)
}

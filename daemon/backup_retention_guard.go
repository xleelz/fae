package daemon

// CanPruneBackup preserves verified recovery coverage at an unchanged plan.
// @ ensures result == (verified && !protected && planMatches)
func CanPruneBackup(verified, protected, planMatches bool) (result bool) {
	return verified && !protected && planMatches
}

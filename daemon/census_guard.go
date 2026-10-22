package daemon

// CanApplyCensus refuses a changed snapshot before an audited retirement.
// @ ensures result == (versionMatches && digestMatches)
func CanApplyCensus(versionMatches, digestMatches bool) (result bool) {
	return versionMatches && digestMatches
}

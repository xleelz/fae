package kernel

// CanResolveUncertain protects explicit recovery from stale identities and
// replacement launch while an old workload may still execute.
// @ ensures ok == (authorized && uncertain && matching && retired && unchanged)
// @ ensures ok ==> authorized && uncertain && matching && retired && unchanged
func CanResolveUncertain(authorized, uncertain, matching, retired, unchanged bool) (ok bool) {
	return authorized && uncertain && matching && retired && unchanged
}

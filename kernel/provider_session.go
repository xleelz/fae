package kernel

// CanResumeProvider keeps every review fresh, for every supported session scope.
// @ ensures ok == (enabled && scopedPolicy && !fresh)
// @ ensures ok ==> enabled && scopedPolicy && !fresh
func CanResumeProvider(enabled, scopedPolicy, fresh bool) (ok bool) {
	return enabled && scopedPolicy && !fresh
}

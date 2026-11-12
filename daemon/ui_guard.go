package daemon

// MayRestartUIChild bounds UI child restarts (FAE-API-033, Appendix A): never
// while the daemon stops, and fewer than limit restarts in the current window.
// @ ensures ok == (!stopping && recent < limit)
func MayRestartUIChild(stopping bool, recent, limit int) (ok bool) {
	return !stopping && recent < limit
}

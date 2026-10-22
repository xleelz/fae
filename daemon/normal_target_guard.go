package daemon

// NormalTargetAuthorized rejects absent adapters and installed policy drift.
// @ ensures result == (installed && matches)
func NormalTargetAuthorized(installed, matches bool) (result bool) {
	return installed && matches
}

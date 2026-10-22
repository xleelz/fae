package daemon

// TargetCommandAuthorized requires a valid pinned policy and an exact command match.
// @ ensures result == (bound && allowed)
func TargetCommandAuthorized(bound, allowed bool) (result bool) {
	return bound && allowed
}

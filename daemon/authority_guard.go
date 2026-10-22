package daemon

// @ ensures result == (authenticated && known && allowed)
func MayPerformAction(authenticated, known, allowed bool) (result bool) {
	return authenticated && known && allowed
}

package loops

// CanDispatch requires both durable inactivity and all schedule bounds.
// @ ensures ok == (active && !inFlight && beforeExpiry && due && belowMaximum)
// @ ensures ok ==> !inFlight && beforeExpiry && belowMaximum
func CanDispatch(active, inFlight, beforeExpiry, due, belowMaximum bool) (ok bool) {
	return active && !inFlight && beforeExpiry && due && belowMaximum
}

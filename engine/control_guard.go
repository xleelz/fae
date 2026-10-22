package engine

// CanChangeTask protects immutable terminal history and forbids unsafe controls
// from changing a dispatch already in flight.
// @ ensures result == (!terminal && (!inFlight || allowsInFlight))
func CanChangeTask(terminal, inFlight, allowsInFlight bool) (result bool) {
	return !terminal && (!inFlight || allowsInFlight)
}

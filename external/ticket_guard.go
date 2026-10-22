package external

// @ ensures result == (!started && !emitted && ordered && live)
func CanEmitTicket(started, emitted, ordered, live bool) (result bool) {
	return !started && !emitted && ordered && live
}

package daemon

// @ requires after >= 0 && floor >= 0
// @ ensures result == (after >= floor)
func CanResumeEvents(after, floor int64) (result bool) {
	return after >= floor
}

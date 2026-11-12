package hostguard

// CanLiftPressureHold is the two-reading rule shared by the pressure hold
// (FAE-HOST-005) and the host-heavy I/O gate (FAE-HOST-010): a first and a
// second reading both strictly below threshold, at least the window apart,
// with no disqualifying reading or over-long silence between them.
// @ ensures result == (firstBelow && secondBelow && spaced && unbroken)
func CanLiftPressureHold(firstBelow, secondBelow, spaced, unbroken bool) (result bool) {
	return firstBelow && secondBelow && spaced && unbroken
}

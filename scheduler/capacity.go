package scheduler

// CanAcquire is shared by role and weighted heavy-op pools. Subtraction avoids
// overflowing when untrusted configuration supplies a very large weight.
// @ requires capacity >= 0 && used >= 0 && used <= capacity
// @ ensures ok ==> weight > 0 && used + weight <= capacity
func CanAcquire(capacity, used, weight int) (ok bool) {
	return weight > 0 && weight <= capacity-used
}

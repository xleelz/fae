package daemon

// CanEnqueueHeavyLease orders FAE-HOST-010 admission: a host-heavy command may
// join the heavy-op lease queue only after its two-reading I/O gate passed.
// @ ensures result == (!hostHeavy || ioGatePassed)
func CanEnqueueHeavyLease(hostHeavy, ioGatePassed bool) (result bool) {
	return !hostHeavy || ioGatePassed
}

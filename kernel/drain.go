package kernel

// CanFinishDrain requires durable collection as well as no active workers.
// A parked uncertain dispatch is still pending and cannot claim a clean drain.
// @ ensures ready == (draining && active == 0 && pending == 0)
// @ ensures ready ==> draining && active == 0 && pending == 0
func CanFinishDrain(draining bool, active, pending int) (ready bool) {
	return draining && active == 0 && pending == 0
}

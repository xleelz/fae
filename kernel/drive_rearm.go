package kernel

// CanRearmDrive prevents concurrent generations and bounds recovery authority.
// @ ensures ok == (!alive && !pending && budget && running)
// @ ensures ok ==> !alive && !pending && budget && running
func CanRearmDrive(alive, pending, budget, running bool) (ok bool) {
	return !alive && !pending && budget && running
}

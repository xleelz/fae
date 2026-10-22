package scheduler

// CanReservePane accounts for both live runtime panes and launches that have
// reserved capacity but do not yet appear in the runtime inventory.
// @ requires limit >= 0 && live >= 0 && pending >= 0
// @ ensures ok ==> live + pending < limit
func CanReservePane(limit, live, pending int) (ok bool) {
	return live < limit && pending < limit-live
}

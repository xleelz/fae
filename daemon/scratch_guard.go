package daemon

// CanRemoveScratch requires the creation marker and verified external backup.
// @ ensures result == (marked && backedUp)
func CanRemoveScratch(marked, backedUp bool) (result bool) { return marked && backedUp }

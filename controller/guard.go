package controller

// CanEnterMaintenance is the production commit-window and overlap guard.
// @ ensures ok == (clearEffects && (!whenClear || clearQA) && (clearOverlap || acknowledged))
// @ ensures ok ==> clearEffects && (!whenClear || clearQA) && (clearOverlap || acknowledged)
func CanEnterMaintenance(clearEffects, whenClear, clearQA, clearOverlap, acknowledged bool) (ok bool) {
	return clearEffects && (!whenClear || clearQA) && (clearOverlap || acknowledged)
}

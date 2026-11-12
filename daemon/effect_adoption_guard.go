package daemon

// CanAutoAdoptEffect: recovery adopts an interrupted effect without an
// operator only from a completed daemon-written receipt (never one rebuilt from
// intent), with retired workloads and exactly remeasured postconditions, and
// only for a local host or VCS effect, never an external one (FAE-ROB-016).
// @ ensures ok ==> localEffect && completedReceipt
// @ ensures ok == (completedReceipt && retired && exactPostcondition && localEffect)
func CanAutoAdoptEffect(completedReceipt, retired, exactPostcondition, localEffect bool) (ok bool) {
	return completedReceipt && retired && exactPostcondition && localEffect
}

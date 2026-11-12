package controller

// GatePasses is the positive-evidence floor shared by every built-in gate: no
// failure, and at least one examined item unless the gate is explicitly not
// applicable (for example a target that declares no frozen region).
// @ requires examined >= 0 && failures >= 0
// @ ensures ok == (failures == 0 && (examined > 0 || !applicable))
// @ ensures ok && applicable ==> examined > 0
func GatePasses(applicable bool, examined, failures int) (ok bool) {
	return failures == 0 && (examined > 0 || !applicable)
}

// MeetsLandingFloor requires positively read memory at or above a configured
// landing floor (FAE-HOST-007). Unreadable memory never satisfies it.
// @ ensures ok == (known && floor > 0 && available >= floor)
func MeetsLandingFloor(known bool, available, floor int64) (ok bool) {
	return known && floor > 0 && available >= floor
}

// CanAdvanceTarget permits the only landing write: every gate passed on the
// exact merged tip, the target still names its starting tip, and the tip
// carries the task's own work.
// @ ensures ok == (gatesPassed && exactTip && targetUnchanged && ownWork)
func CanAdvanceTarget(gatesPassed, exactTip, targetUnchanged, ownWork bool) (ok bool) {
	return gatesPassed && exactTip && targetUnchanged && ownWork
}

// MayDeployGated accepts a deployment source only when the gated reference's
// current tip carries a durable landing proof for that exact commit.
// @ ensures ok == (gatedNamespace && proofRecorded && proofTip)
func MayDeployGated(gatedNamespace, proofRecorded, proofTip bool) (ok bool) {
	return gatedNamespace && proofRecorded && proofTip
}

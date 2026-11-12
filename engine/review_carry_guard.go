package engine

// CanCarryReview admits approvals across a reconcile only for a live, clean,
// patch-identical merge whose reviewed and reconciled trees are both measured
// (FAE-VCS-005a, FAE-WF-071): the reviewed diff is unchanged, so review
// evidence carries while tests and QA re-run on the exact committed tree.
func CanCarryReview(live, clean, patchIdentical, measured bool) bool {
	return live && clean && patchIdentical && measured
}

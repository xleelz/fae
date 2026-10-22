package effects

// CanReconcileCommitIndex prevents replacing foreign staging or a changed head.
// @ ensures result == (trustedIndex && expectedHead)
func CanReconcileCommitIndex(trustedIndex, expectedHead bool) (result bool) {
	return trustedIndex && expectedHead
}

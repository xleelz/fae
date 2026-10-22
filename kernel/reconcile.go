package kernel

// CanApplyReconcile permits replacement of only a known pre/post reconcile tree.
// @ ensures allowed == (tipMatches && knownHead && knownTree && evidenceValid)
// @ ensures allowed ==> tipMatches && knownHead && knownTree && evidenceValid
func CanApplyReconcile(tipMatches, knownHead, knownTree, evidenceValid bool) (allowed bool) {
	return tipMatches && knownHead && knownTree && evidenceValid
}

package daemon

// CanRestartWorkspace requires both broker-proved HEAD and durable tree identity.
// @ ensures result == (headKnown && treeKnown)
func CanRestartWorkspace(headKnown, treeKnown bool) (result bool) {
	return headKnown && treeKnown
}

package daemon

// CanPrepareReviewWorkspace binds approved package verification to the owned task workspace.
// @ ensures result == (approvedPackage && exactBinding)
func CanPrepareReviewWorkspace(approvedPackage, exactBinding bool) (result bool) {
	return approvedPackage && exactBinding
}

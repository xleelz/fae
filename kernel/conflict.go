package kernel

// CanKeepOwnHunk admits a resolved hunk only if neither its original additions
// nor its original deletion effects were lost. Inputs are broker measurements.
// @ ensures allowed == (additionsRetained && deletionsRetained)
// @ ensures allowed ==> additionsRetained && deletionsRetained
func CanKeepOwnHunk(additionsRetained, deletionsRetained bool) (allowed bool) {
	return additionsRetained && deletionsRetained
}

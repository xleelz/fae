package daemon

// CanCommitWithoutRed is the no-red commit floor (FAE-WF-073, FAE-ROB-013):
// frozen RED evidence always suffices; without it a declared classifier must
// find no production code, and without a classifier no-red cannot be declared.
// @ ensures result == (hasRed || (classified && !codeBearing) || (!classified && !declaredNoRed))
func CanCommitWithoutRed(hasRed, declaredNoRed, classified, codeBearing bool) (result bool) {
	return hasRed || (classified && !codeBearing) || (!classified && !declaredNoRed)
}

// CanCommitTree requires a QA proof for the exact committed tree whenever the
// pinned target declares required tests or a compile check (FAE-WF-085).
// @ ensures result == (!checksDeclared || proven)
func CanCommitTree(checksDeclared, proven bool) (result bool) {
	return !checksDeclared || proven
}

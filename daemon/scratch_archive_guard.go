package daemon

// CanRemoveScratchArchive protects the completed canonical recovery copy.
// @ ensures result == (completedJournal && exactSurvivor && unreferencedSource)
func CanRemoveScratchArchive(completedJournal, exactSurvivor, unreferencedSource bool) (result bool) {
	return completedJournal && exactSurvivor && unreferencedSource
}

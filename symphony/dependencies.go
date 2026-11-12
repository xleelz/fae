package symphony

// BlockersCleared uses provider-derived completion when available, including
// blockers in another team's differently named completion states. Missing
// metadata never establishes that a dependency is finished.
func BlockersCleared(issue Issue, terminalStates []string) bool {
	for _, blocker := range issue.BlockedBy {
		if blocker.Terminal != nil {
			if !*blocker.Terminal {
				return false
			}
			continue
		}
		if blocker.State == nil || !StateInList(*blocker.State, terminalStates) {
			return false
		}
	}
	return true
}

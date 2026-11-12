package daemon

// SubmissionModeComplete is the submission mode-completeness rule (FAE-CLI-011,
// FAE-MODE-004): an explicitly Normal submission names exactly one of dry run
// and commit-external; a Local or unstated one names neither. An unstated mode
// takes the instance default disposition, which is never live.
// @ ensures result == (!(dryRun && commitExternal) && (statedNormal == (dryRun || commitExternal)))
func SubmissionModeComplete(statedNormal, dryRun, commitExternal bool) (result bool) {
	return !(dryRun && commitExternal) && statedNormal == (dryRun || commitExternal)
}

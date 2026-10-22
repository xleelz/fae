package worktree

// CanCleanupArchived requires both archive identity and verified backup authority.
// @ ensures result == (archiveMatches && backupVerified)
func CanCleanupArchived(archiveMatches, backupVerified bool) (result bool) {
	return archiveMatches && backupVerified
}

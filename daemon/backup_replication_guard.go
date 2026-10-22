package daemon

// CanConfirmBackupReplication requires both the exact receipt and fetched archive proof.
// @ ensures result == (identityMatched && contentMatched)
func CanConfirmBackupReplication(identityMatched, contentMatched bool) (result bool) {
	return identityMatched && contentMatched
}

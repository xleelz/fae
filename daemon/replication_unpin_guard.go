package daemon

// CanUnpinReplication requires complete proof and no outstanding selected transfer.
// @ ensures result == (allConfirmed && remoteVerified && noPendingClaim)
func CanUnpinReplication(allConfirmed, remoteVerified, noPendingClaim bool) (result bool) {
	return allConfirmed && remoteVerified && noPendingClaim
}

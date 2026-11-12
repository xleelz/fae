package ui

// MayListen admits a UI listener only on a literal, specified loopback address
// (FAE-API-030). A hostname, a wildcard or any other interface fails closed.
// @ ensures ok == (literal && loopback && !unspecified)
func MayListen(literal, loopback, unspecified bool) (ok bool) {
	return literal && loopback && !unspecified
}

// MayServeRequest is the per-request gate (FAE-API-031, FAE-ARC-006): a loopback
// peer, addressing the listener by its own loopback host, presenting the token.
// @ ensures ok == (loopbackPeer && expectedHost && tokenMatches)
func MayServeRequest(loopbackPeer, expectedHost, tokenMatches bool) (ok bool) {
	return loopbackPeer && expectedHost && tokenMatches
}

// MayWrite admits a UI-originated mutation only with the daemon-issued write
// capability (FAE-API-032); the daemon re-checks it on the RPC itself.
// @ ensures ok == (authorized && writeCapability)
func MayWrite(authorized, writeCapability bool) (ok bool) {
	return authorized && writeCapability
}

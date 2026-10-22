package kernel

// CanRetryStall requires both an explicit restricted effects contract and
// post-retirement measurement; tree equality alone never proves safe replay.
// @ ensures ok == (restrictedContract && retired && unchanged && firstSession)
// @ ensures ok ==> restrictedContract && retired && unchanged && firstSession
func CanRetryStall(restrictedContract, retired, unchanged, firstSession bool) (ok bool) {
	return restrictedContract && retired && unchanged && firstSession
}

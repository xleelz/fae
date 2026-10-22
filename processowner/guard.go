package processowner

// CanTerminate is the actual fail-closed orphan ownership decision.
// @ ensures ok == (ownerMatches && identityMatches && groupMatches && privateRecord)
// @ ensures ok ==> ownerMatches && identityMatches && groupMatches && privateRecord
func CanTerminate(ownerMatches, identityMatches, groupMatches, privateRecord bool) (ok bool) {
	return ownerMatches && identityMatches && groupMatches && privateRecord
}

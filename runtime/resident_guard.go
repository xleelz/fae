package runtime

// CanReuseResident is the actual resident transport identity admission guard.
// @ ensures ok == (identityMatches && live && !fresh)
// @ ensures ok ==> identityMatches && live && !fresh
func CanReuseResident(identityMatches, live, fresh bool) (ok bool) {
	return identityMatches && live && !fresh
}

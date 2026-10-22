package daemon

// AdapterBindingValid prevents revision or descriptor drift during resolution.
// @ ensures result == (revisionMatches && hashMatches)
func AdapterBindingValid(revisionMatches, hashMatches bool) (result bool) {
	return revisionMatches && hashMatches
}

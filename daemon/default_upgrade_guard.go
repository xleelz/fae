package daemon

// MayInstallShippedDefault permits installing the shipped default as a new
// revision over the latest stored one only when that revision differs and is
// unusable (fails current validation) or an older builtin generation. A
// retired default and a valid operator revision are never replaced.
// @ ensures result == (!retired && differs && (invalid || (builtin && older)))
func MayInstallShippedDefault(retired, differs, invalid, builtin, older bool) (result bool) {
	return !retired && differs && (invalid || (builtin && older))
}

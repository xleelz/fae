package commandshim

// CanExecute is checked after concrete path and role measurements and before
// entering a command process. Nested shim calls never reuse a bearer lease.
// @ ensures allowed == (roleAllowed && scopeValid && outputSafe && leaseHeld && !nested)
// @ ensures allowed ==> roleAllowed && scopeValid && outputSafe && leaseHeld && !nested
func CanExecute(roleAllowed, scopeValid, outputSafe, leaseHeld, nested bool) (allowed bool) {
	return roleAllowed && scopeValid && outputSafe && leaseHeld && !nested
}

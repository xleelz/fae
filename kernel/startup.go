package kernel

// CanRetryStartup requires positive nonexecution proof and completed retirement.
// @ ensures ok == (notExecuted && retired && firstSession)
// @ ensures ok ==> notExecuted && retired && firstSession
func CanRetryStartup(notExecuted, retired, firstSession bool) (ok bool) {
	return notExecuted && retired && firstSession
}

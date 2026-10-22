// Package kernel holds small non-bypassable execution safety decisions.
package kernel

// CanComplete is the shared completion admission predicate. Callers derive the
// inputs from the durable job and validated result, never from agent declarations.
// @ ensures allowed == (!terminal && pending && tokenMatches && evidenceValid)
// @ ensures allowed ==> !terminal && pending && tokenMatches && evidenceValid
func CanComplete(terminal, pending, tokenMatches, evidenceValid bool) (allowed bool) {
	return !terminal && pending && tokenMatches && evidenceValid
}

package daemon

// CanEnactProphet is the authority boundary between a proposed action and a verb.
// @ ensures res == (directed && supported)
func CanEnactProphet(directed bool, supported bool) (res bool) {
	return directed && supported
}

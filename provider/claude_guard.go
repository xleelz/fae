package provider

// @ ensures result == (identity && surface && outcome)
func CanAcceptClaudeTurn(identity, surface, outcome bool) (result bool) {
	return identity && surface && outcome
}

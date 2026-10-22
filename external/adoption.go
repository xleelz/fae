package external

// @ ensures result == (identity && pins && named && performed)
func CanAdopt(identity, pins, named, performed bool) (result bool) {
	return identity && pins && named && performed
}

package kernel

// @ ensures ok == (authorized && retired && exactPostcondition && trustedReceipt)
// @ ensures ok ==> authorized && retired && exactPostcondition && trustedReceipt
func CanAdoptEffect(authorized, retired, exactPostcondition, trustedReceipt bool) (ok bool) {
	return authorized && retired && exactPostcondition && trustedReceipt
}

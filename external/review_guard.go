package external

// @ ensures result == (requiredPass && commentsClear && knownStatus)
func MayAdvanceReview(requiredPass, commentsClear, knownStatus bool) (result bool) {
	return requiredPass && commentsClear && knownStatus
}

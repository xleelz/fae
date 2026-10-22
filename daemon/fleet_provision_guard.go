package daemon

// CanFinishFleetProvision requires the durable attempt, matched receipt and green baseline.
// @ ensures result == (attemptRecorded && receiptMatched && baselineGreen)
func CanFinishFleetProvision(attemptRecorded, receiptMatched, baselineGreen bool) (result bool) {
	return attemptRecorded && receiptMatched && baselineGreen
}

package completion

// Releasing a local retention pin follows fresh remote proof and a check that
// no pending schedule still owns that source.
func BackupUnpinWaitsForProofAndNoPendingClaim() {
	proof := make(chan int)
	unclaimed := make(chan int)
	unpinned := make(chan int)
	go func() { proof <- 1 }()
	go func() { unclaimed <- 1 }()
	go func() { <-proof; <-unclaimed; unpinned <- 1 }()
	<-unpinned
}

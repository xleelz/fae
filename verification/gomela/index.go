package completion

// Commit completion waits for the branch update and then index reconciliation.
// An index synchronization error cannot be reported as a completed commit.
func CommitReceiptAfterIndexReconciliation() {
	branchCommitted := make(chan int)
	indexReconciled := make(chan int)
	receipt := make(chan int)
	go func() { branchCommitted <- 1 }()
	go func() { <-branchCommitted; indexReconciled <- 1 }()
	go func() { <-indexReconciled; receipt <- 1 }()
	<-receipt
}

package completion

// A durable commit receipt is collected before a restarted execution can use
// its HEAD. Reconciliation retains the original base on a separate channel.
func RestartAfterCommitReceipt() {
	receipt := make(chan int)
	originalBase := make(chan int)
	executionHead := make(chan int)
	go func() { originalBase <- 1 }()
	go func() { receipt <- 2 }()
	go func() { h := <-receipt; executionHead <- h }()
	<-executionHead
	<-originalBase
}

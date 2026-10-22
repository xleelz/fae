package completion

// StartupProofBeforeRetry models nonexecution, retirement and durable acknowledgment.
func StartupProofBeforeRetry() {
	proof := make(chan int)
	retired := make(chan int)
	committed := make(chan int)
	go startupProver(proof, retired)
	go startupCommit(proof, retired, committed)
	<-committed
}
func startupProver(proof, retired chan int)            { proof <- 1; retired <- 1 }
func startupCommit(proof, retired, committed chan int) { <-proof; <-retired; committed <- 1 }

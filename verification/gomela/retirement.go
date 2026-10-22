package completion

// The successor edge handoff is acknowledged before retirement is published.
func RetireTransfersBeforePublish() {
	edges := make(chan int)
	ack := make(chan int)
	retired := make(chan int)
	go func() { edges <- 1; <-ack; retired <- 1 }()
	go func() { <-edges; ack <- 1 }()
	<-retired
}

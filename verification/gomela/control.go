package completion

// A completion is retained while an operator hold is committed. Later context
// is published only after the current brief has been frozen.
func ControlHoldCollectsBeforeResume() {
	frozen := make(chan int)
	held := make(chan int)
	completion := make(chan int)
	collected := make(chan int)
	go func() { frozen <- 1; completion <- 1 }()
	go func() { <-frozen; held <- 1 }()
	go func() { <-held; <-completion; collected <- 1 }()
	<-collected
}

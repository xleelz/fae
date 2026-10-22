package completion

func FleetReceiptBeforeBaselineReady() {
	intent := make(chan int)
	receipt := make(chan int)
	baseline := make(chan int)
	ready := make(chan int)
	go func() { intent <- 1 }()
	go func() { <-intent; receipt <- 1 }()
	go func() { <-receipt; baseline <- 1 }()
	go func() { <-baseline; ready <- 1 }()
	<-ready
}

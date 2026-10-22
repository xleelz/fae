package completion

// CrashResolutionBarrier requires retirement and explicit operator resolution
// before a persisted replacement dispatch may be launched.
func CrashResolutionBarrier() {
	retired := make(chan int)
	authority := make(chan int)
	committed := make(chan int)
	go func() { retired <- 1 }()
	go func() { authority <- 1 }()
	go func() { <-retired; <-authority; committed <- 1 }()
	<-committed
}

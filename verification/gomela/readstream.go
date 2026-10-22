package completion

func IsolatedReadCompletion() {
	slow := make(chan int, 1)
	fast := make(chan int)
	done := make(chan int)
	go func() { slow <- 1; fast <- 1; done <- 1 }()
	<-fast
	<-done
	<-slow
}

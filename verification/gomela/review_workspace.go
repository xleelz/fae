package completion

func WorkspaceReadyAfterBaseline() {
	approved := make(chan int)
	ready := make(chan int)
	go func() { approved <- 1 }()
	go func() { <-approved; ready <- 1 }()
	<-ready
}

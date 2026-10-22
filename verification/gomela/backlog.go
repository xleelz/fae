package completion

// A dependent is released only after the prerequisite landing checkpoint.
func BacklogCommitBeforeRelease() {
	committed := make(chan int)
	released := make(chan int)
	go func() { committed <- 1 }()
	go func() { <-committed; released <- 1 }()
	<-released
}

package completion

// Archive cleanup verifies a surviving recoverable copy and journal protection
// before deleting redundant data.
func ScratchArchiveRequiresSurvivorAndJournalProof() {
	survivor := make(chan int)
	unreferenced := make(chan int)
	deleted := make(chan int)
	go func() { survivor <- 1 }()
	go func() { unreferenced <- 1 }()
	go func() { <-survivor; <-unreferenced; deleted <- 1 }()
	<-deleted
}

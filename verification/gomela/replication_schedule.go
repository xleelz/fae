package completion

// A replication schedule pins its source and destination before entering the
// upload attempt. Recovery consumes the original receipt before advancing.
func ReplicationSchedulePinsBeforeAttemptAndAdvance() {
	pinned := make(chan int)
	attempted := make(chan int)
	verified := make(chan int)
	advanced := make(chan int)
	go func() { pinned <- 1 }()
	go func() { <-pinned; attempted <- 1 }()
	go func() { <-attempted; verified <- 1 }()
	go func() { <-verified; advanced <- 1 }()
	<-advanced
}

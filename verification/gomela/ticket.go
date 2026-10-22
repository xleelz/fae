package completion

// Ordered emission checkpoints Started before mutation; restart only queries.
func TicketOutboxRecovery() {
	ordered := make(chan int)
	started := make(chan int)
	performed := make(chan int)
	query := make(chan int)
	emitted := make(chan int)
	go func() { ordered <- 1 }()
	go func() { <-ordered; started <- 1 }()
	go func() { <-started; performed <- 1 }()
	go func() { <-performed; query <- 1 }()
	go func() { <-query; emitted <- 1 }()
	<-emitted
}

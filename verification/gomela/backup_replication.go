package completion

// Transfer follows a durable intent. Confirmation consumes both the exact
// destination receipt and a verified content round trip.
func BackupReplicationVerifiesBeforeReceipt() {
	intent := make(chan int)
	transferred := make(chan int)
	receipt := make(chan int)
	content := make(chan int)
	confirmed := make(chan int)
	go func() { intent <- 1 }()
	go func() { <-intent; transferred <- 1 }()
	go func() { <-transferred; receipt <- 1 }()
	go func() { <-receipt; content <- 1 }()
	go func() { <-content; confirmed <- 1 }()
	<-confirmed
}

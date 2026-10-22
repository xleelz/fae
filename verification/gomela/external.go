package completion

// Recovery queries the adapter after durable intent and completed remote effect.
// There is no second mutation path after the crash boundary.
func ExternalQueryBeforeAdoption() {
	intent := make(chan int)
	performed := make(chan int)
	query := make(chan int)
	receipt := make(chan int)
	adopted := make(chan int)
	go func() { intent <- 1 }()
	go func() { <-intent; performed <- 1 }()
	go func() { <-performed; query <- 1 }()
	go func() { <-query; receipt <- 1 }()
	go func() { <-receipt; adopted <- 1 }()
	<-adopted
}

package completion

// A new provider turn observes the durable context acknowledgment before
// acquiring the same scope; only one turn owns the scope at a time.
func PersistentProviderCommit() {
	scope := make(chan int, 1)
	commit := make(chan int)
	ack := make(chan int)
	done := make(chan int)
	scope <- 1
	go persistentProviderTurn(scope, commit, ack, done)
	go persistentProviderWriter(commit, ack)
	<-done
	<-scope
}
func persistentProviderTurn(scope, commit, ack, done chan int) {
	<-scope
	commit <- 1
	<-ack
	scope <- 1
	done <- 1
}
func persistentProviderWriter(commit, ack chan int) { <-commit; ack <- 1 }

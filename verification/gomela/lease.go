package completion

// LeaseRelease models two competing workers and a single capacity token. The
// token is held until an acknowledged process exit, then returned exactly once.
// Persistence and process execution are abstractions; identity is proved by Gobra.
func LeaseRelease() {
	token := make(chan int, 1)
	done := make(chan int, 2)
	token <- 1
	go leasedProcess(token, done)
	go leasedProcess(token, done)
	<-done
	<-done
}
func leasedProcess(token, done chan int) {
	<-token
	exited := make(chan int)
	go leaseChild(exited)
	<-exited
	token <- 1
	done <- 1
}
func leaseChild(exited chan int) { close(exited) }

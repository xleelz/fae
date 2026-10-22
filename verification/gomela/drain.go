package completion

// DrainBeforeRestart models two already-launched turns. Draining closes launch
// admission; each completion is durably acknowledged before restart is released.
func DrainBeforeRestart() {
	results := make(chan int, 2)
	persist := make(chan int)
	ack := make(chan int)
	restart := make(chan int)
	go drainedWorker(results)
	go drainedWorker(results)
	go drainedWriter(persist, ack)
	go drainedCollector(results, persist, ack, restart)
	<-restart
}
func drainedWorker(results chan int) { results <- 1 }
func drainedWriter(persist, ack chan int) {
	<-persist
	ack <- 1
	<-persist
	ack <- 1
}
func drainedCollector(results, persist, ack, restart chan int) {
	<-results
	persist <- 1
	<-ack
	<-results
	persist <- 1
	<-ack
	close(restart)
}

package completion

// ControllerLifecycle requires the maintenance marker before drain, terminal
// drain before stop, and post-start acceptance before marker removal.
func ControllerLifecycle() {
	marker := make(chan int)
	drained := make(chan int)
	stopped := make(chan int)
	accepted := make(chan int)
	done := make(chan int)
	go controllerWorker(marker, drained, stopped, accepted, done)
	marker <- 1
	<-drained
	stopped <- 1
	<-accepted
	done <- 1
}
func controllerWorker(marker, drained, stopped, accepted, done chan int) {
	<-marker
	drained <- 1
	<-stopped
	accepted <- 1
	<-done
}

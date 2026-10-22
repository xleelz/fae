package completion

// PersistBeforeDispatch models the daemon's durable acknowledgment boundaries.
// SQL commit success is abstracted as an acknowledgment, not verified by SPIN.
func PersistBeforeDispatch() {
	persist := make(chan int)
	acknowledged := make(chan int)
	result := make(chan int, 1)
	done := make(chan int)
	go durableWriter(persist, acknowledged, done)
	persist <- 1
	<-acknowledged
	go durableWorker(result)
	<-result
	persist <- 2
	<-done
}
func durableWriter(request, acknowledged, done chan int) {
	<-request
	acknowledged <- 1
	<-request
	close(done)
}
func durableWorker(result chan int) { result <- 1 }

// ShutdownWithTwoWorkers models the production bounded completion buffer at
// MaxInFlight=2: cancellation can finish both workers without a live collector.
func ShutdownWithTwoWorkers() {
	result := make(chan int, 2)
	cancel := make(chan int, 2)
	go cancelledWorker(result, cancel)
	go cancelledWorker(result, cancel)
	cancel <- 1
	cancel <- 1
}
func cancelledWorker(result, cancel chan int) { <-cancel; result <- 1 }

package completion

// DynamicPaneLimitUpdate models a serialized cap update acknowledged before a
// waiting launch retries. Existing reservations retire independently of lowering.
func DynamicPaneLimitUpdate() {
	lock := make(chan int, 1)
	updated := make(chan int)
	retired := make(chan int)
	done := make(chan int, 2)
	lock <- 1
	go paneUpdate(lock, updated, done)
	go paneRetry(lock, updated, retired, done)
	close(retired)
	<-done
	<-done
}
func paneUpdate(lock, updated, done chan int) { <-lock; lock <- 1; close(updated); done <- 1 }
func paneRetry(lock, updated, retired, done chan int) {
	<-updated
	<-retired
	<-lock
	lock <- 1
	done <- 1
}

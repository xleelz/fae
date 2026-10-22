package completion

// PaneReservationLifecycle models serialization of live census and pending
// reservations, and retirement before another owner may reuse the final slot.
func PaneReservationLifecycle() {
	lock := make(chan int, 1)
	retired := make(chan int)
	done := make(chan int, 2)
	lock <- 1
	go paneFirst(lock, retired, done)
	go paneSecond(lock, retired, done)
	<-done
	<-done
}
func paneFirst(lock, retired, done chan int)  { <-lock; lock <- 1; close(retired); done <- 1 }
func paneSecond(lock, retired, done chan int) { <-retired; <-lock; lock <- 1; done <- 1 }

package completion

// DynamicHostDrain models lowering capacity while a holder remains live. The
// waiter cannot acquire the shared permit until the holder's retirement signal.
func DynamicHostDrain() {
	permit := make(chan int, 1)
	update := make(chan int)
	retired := make(chan int)
	done := make(chan int, 2)
	go func() { close(update); <-retired; permit <- 1; done <- 1 }()
	go func() { <-update; <-permit; done <- 1 }()
	close(retired)
	<-done
	<-done
}

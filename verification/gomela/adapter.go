package completion

// Every external effect consumes policy validation before completing its effect.
func AdapterPinBeforeEffect() {
	validated := make(chan int)
	executed := make(chan int)
	go func() { validated <- 1 }()
	go func() { <-validated; executed <- 1 }()
	<-executed
}

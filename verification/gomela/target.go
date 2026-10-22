package completion

// Every command consumes policy validation before completing its effect.
func TargetPolicyBeforeCommand() {
	validated := make(chan int)
	executed := make(chan int)
	go func() { validated <- 1 }()
	go func() { <-validated; executed <- 1 }()
	<-executed
}

package completion

func SchemaBeforeEffect() {
	discovered := make(chan int)
	validated := make(chan int)
	effect := make(chan int)
	go func() { discovered <- 1 }()
	go func() { <-discovered; validated <- 1 }()
	go func() { <-validated; effect <- 1 }()
	<-effect
}

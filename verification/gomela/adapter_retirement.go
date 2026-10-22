package completion

// Admission and retirement share an exclusive commit boundary.
func AdapterRetirementSerialized() {
	gate := make(chan int, 1)
	done := make(chan int)
	gate <- 1
	go func() { <-gate; gate <- 1; done <- 1 }()
	go func() { <-gate; gate <- 1; done <- 1 }()
	<-done
	<-done
}

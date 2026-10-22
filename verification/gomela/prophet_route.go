package completion

func ProphetObservationAndRetirementBeforeFailover() {
	observed := make(chan int)
	retired := make(chan int)
	next := make(chan int)
	go func() { observed <- 1 }()
	go func() { retired <- 1 }()
	go func() { <-observed; <-retired; next <- 1 }()
	<-next
}

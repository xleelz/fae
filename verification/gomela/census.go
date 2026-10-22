package completion

func CensusSnapshotBeforeRetirement() {
	checked := make(chan int)
	retired := make(chan int)
	go func() { checked <- 1 }()
	go func() { <-checked; retired <- 1 }()
	<-retired
}

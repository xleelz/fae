package completion

func SerializedDriveGenerations() {
	retired := make(chan int)
	rearmed := make(chan int)
	go func() { retired <- 1; <-rearmed }()
	<-retired
	rearmed <- 1
}

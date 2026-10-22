package completion

func ScratchRemovalWaitsForStopAndBackup() {
	stopped := make(chan int)
	backup := make(chan int)
	removed := make(chan int)
	go func() { stopped <- 1 }()
	go func() { <-stopped; backup <- 1 }()
	go func() { <-backup; removed <- 1 }()
	<-removed
}

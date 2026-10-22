package completion

// Removal waits for both independently verified copies.
func CleanupWaitsForBackupAndArchive() {
	backup := make(chan int)
	archive := make(chan int)
	removed := make(chan int)
	go func() { backup <- 1 }()
	go func() { archive <- 1 }()
	go func() { <-backup; <-archive; removed <- 1 }()
	<-removed
}

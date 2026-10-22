package completion

// Removal waits for both a verified surviving backup and unchanged source proof.
func BackupRetentionRequiresSurvivorAndIdentity() {
	survivor := make(chan int)
	unchanged := make(chan int)
	removed := make(chan int)
	go func() { survivor <- 1 }()
	go func() { unchanged <- 1 }()
	go func() { <-survivor; <-unchanged; removed <- 1 }()
	<-removed
}

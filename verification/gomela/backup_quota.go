package completion

// One serialized storage reservation protects both creation and replication
// staging; neither writer can occupy the capacity token concurrently.
func BackupQuotaSerializesStorageWriters() {
	capacity := make(chan int, 1)
	done := make(chan int)
	capacity <- 1
	go func() { <-capacity; capacity <- 1; done <- 1 }()
	go func() { <-capacity; capacity <- 1; done <- 1 }()
	<-done
	<-done
}

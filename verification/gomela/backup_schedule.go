package completion

// A recurring backup must have authorization and a durable occurrence claim
// before creating new instance data.
func BackupScheduleClaimsBeforeCreation() {
	authorized := make(chan int)
	claimed := make(chan int)
	created := make(chan int)
	completed := make(chan int)
	go func() { authorized <- 1 }()
	go func() { <-authorized; claimed <- 1 }()
	go func() { <-claimed; created <- 1 }()
	go func() { <-created; completed <- 1 }()
	<-completed
}

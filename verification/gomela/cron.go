package completion

func CronAdmissionBeforeCursor() {
	admitted := make(chan int)
	checkpoint := make(chan int)
	go cronCommit(admitted, checkpoint)
	admitted <- 1
	<-checkpoint
}
func cronCommit(admitted, checkpoint chan int) { <-admitted; checkpoint <- 1 }

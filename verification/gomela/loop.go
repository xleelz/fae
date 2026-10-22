package completion

func LoopCheckpointBeforePrompt() {
	checkpoint := make(chan int)
	completed := make(chan int)
	go loopPrompt(checkpoint, completed)
	checkpoint <- 1
	<-completed
}
func loopPrompt(checkpoint, completed chan int) { <-checkpoint; completed <- 1 }

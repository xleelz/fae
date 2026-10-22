// Package completion is a bounded model of Scripted.Dispatch's buffered outcome
// protocol; timer/context implementations and payload processing are abstracted.
package completion

func Completion() {
	result := make(chan int, 1)
	cancel := make(chan int, 1)
	timer := make(chan int, 1)
	cancel <- 1
	timer <- 1
	go complete(result, cancel, timer)
	<-result
}
func complete(result, cancel, timer chan int) {
	select {
	case <-cancel:
		result <- 0
	case <-timer:
		result <- 1
	}
	close(result)
}
func AbandonedCollection() {
	result := make(chan int, 1)
	go abandoned(result)
}
func abandoned(result chan int) { result <- 1; close(result) }

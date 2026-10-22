package completion

// StallRetirementBeforeRetry models the acknowledgment barrier: a replacement
// can launch only after old worker retirement and durable retry persistence.
func StallRetirementBeforeRetry() {
	stop := make(chan int)
	retired := make(chan int)
	persist := make(chan int)
	committed := make(chan int)
	replacement := make(chan int)
	go stallWorker(stop, retired)
	go stallWriter(persist, committed)
	go stallRecover(stop, retired, persist, committed, replacement)
	<-replacement
}
func stallWorker(stop, retired chan int)      { <-stop; retired <- 1 }
func stallWriter(persist, committed chan int) { <-persist; committed <- 1 }
func stallRecover(stop, retired, persist, committed, replacement chan int) {
	stop <- 1
	<-retired
	persist <- 1
	<-committed
	replacement <- 1
}

package completion

// IdleRetirement retains context before retirement, then reports completion.
// The daemon must collect the retirement acknowledgement before capacity is reused.
func IdleRetirement() {
	contextSaved := make(chan int)
	retire := make(chan int)
	retired := make(chan int)
	go idleHandoffWriter(contextSaved)
	go idleRetirer(contextSaved, retire, retired)
	retire <- 1
	<-retired
}
func idleHandoffWriter(saved chan int)            { saved <- 1 }
func idleRetirer(saved, retire, retired chan int) { <-saved; <-retire; retired <- 1 }

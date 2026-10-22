package completion

// ShimLeaseRetirement abstracts one broker slot, a child shim refusal, and
// retirement acknowledgment before the slot is returned. PATH/OS behavior is
// checked separately by native command tests.
func ShimLeaseRetirement() {
	lease := make(chan int, 1)
	nested := make(chan int)
	refused := make(chan int)
	retired := make(chan int)
	lease <- 1
	go shimChild(nested, refused)
	go shimParent(lease, nested, refused, retired)
	<-retired
	<-lease
}
func shimChild(nested, refused chan int) { <-nested; refused <- 1 }
func shimParent(lease, nested, refused, retired chan int) {
	<-lease
	nested <- 1
	<-refused
	lease <- 1
	close(retired)
}

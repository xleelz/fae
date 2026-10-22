package completion

// EffectReceiptBeforeAdoption models durable intent, a single external effect,
// its persisted result, and explicit operator adoption after receipt delivery.
func EffectReceiptBeforeAdoption() {
	intent := make(chan int)
	performed := make(chan int)
	receipt := make(chan int)
	authority := make(chan int)
	adopted := make(chan int)
	go func() { intent <- 1 }()
	go func() { <-intent; performed <- 1 }()
	go func() { <-performed; receipt <- 1 }()
	go func() { authority <- 1 }()
	go func() { <-receipt; <-authority; adopted <- 1 }()
	<-adopted
}

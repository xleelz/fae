package completion

func AuthorityBeforeMutation() {
	authenticated := make(chan int)
	permitted := make(chan int)
	committed := make(chan int)
	go func() { authenticated <- 1 }()
	go func() { <-authenticated; permitted <- 1 }()
	go func() { <-permitted; committed <- 1 }()
	<-committed
}

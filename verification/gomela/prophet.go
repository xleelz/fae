package completion

func ProphetDirectionBeforeAction() {
	direction := make(chan int)
	action := make(chan int)
	reply := make(chan int)
	go func() { direction <- 1 }()
	go func() { <-direction; action <- 1 }()
	go func() { <-action; reply <- 1 }()
	<-reply
}

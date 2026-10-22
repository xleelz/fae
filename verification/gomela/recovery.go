package completion

// DurableLeaseRecovery models immutable complete retirement acknowledgment
// before recovery returns the resource token after owner death.
func DurableLeaseRecovery(){
 death:=make(chan int)
 retired:=make(chan int)
 capacity:=make(chan int,1)
 done:=make(chan int)
 go recoveryWatch(death,retired)
 go recoveryLease(retired,capacity)
 go recoveryWaiter(capacity,done)
 close(death)
 <-done
}
func recoveryWatch(death,retired chan int){<-death;close(retired)}
func recoveryLease(retired,capacity chan int){<-retired;capacity<-1}
func recoveryWaiter(capacity,done chan int){<-capacity;close(done)}

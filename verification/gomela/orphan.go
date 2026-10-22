package completion

// ParentDeathRetirement models parent death notification, watchdog termination,
// complete group exit acknowledgement, and only then capacity reuse. OS identity
// and the census are abstract inputs, not a proof of OS behavior.
func ParentDeathRetirement() {
 death:=make(chan int)
 groupGone:=make(chan int)
 reusable:=make(chan int,1)
 done:=make(chan int)
 go orphanWatch(death,groupGone)
 go orphanReclaim(groupGone,reusable)
 go orphanNext(reusable,done)
 close(death)
 <-done
}
func orphanWatch(death,groupGone chan int){<-death;close(groupGone)}
func orphanReclaim(groupGone,reusable chan int){<-groupGone;reusable<-1}
func orphanNext(reusable,done chan int){<-reusable;close(done)}

package completion

// A scheduled prune is authorized and durably claimed before a fresh plan is
// accepted; completion cannot precede the prune acknowledgment.
func RetentionScheduleClaimsBeforePruning() {
	authorized := make(chan int)
	claimed := make(chan int)
	planned := make(chan int)
	pruned := make(chan int)
	go func() { authorized <- 1 }()
	go func() { <-authorized; claimed <- 1 }()
	go func() { <-claimed; planned <- 1 }()
	go func() { <-planned; pruned <- 1 }()
	<-pruned
}

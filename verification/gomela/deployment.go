package completion

// DeploymentAcceptance requires source gating and integration acceptance before resume.
func DeploymentAcceptance() {
	gated := make(chan int)
	stopped := make(chan int)
	accepted := make(chan int)
	done := make(chan int)
	go deploymentWorker(gated, stopped, accepted, done)
	gated <- 1
	<-stopped
	accepted <- 1
	<-done
}
func deploymentWorker(gated, stopped, accepted, done chan int) {
	<-gated
	stopped <- 1
	<-accepted
	done <- 1
}

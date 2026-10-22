package completion

// OwnedPaneLifecycle abstracts durable registration, permit release, bridge
// completion, then pane retirement before server shutdown. Each process has one
// owner. OS identity validation is separately checked by Gobra and native tests.
func OwnedPaneLifecycle() {
	registered := make(chan int)
	permit := make(chan int)
	completed := make(chan int)
	paneGone := make(chan int)
	serverGone := make(chan int)
	go muxOwner(registered, permit, completed, paneGone)
	go muxPane(permit, completed, paneGone)
	go muxServer(paneGone, serverGone)
	close(registered)
	<-serverGone
}
func muxOwner(registered, permit, completed, paneGone chan int) {
	<-registered
	close(permit)
	<-completed
	close(paneGone)
}
func muxPane(permit, completed, paneGone chan int) { <-permit; close(completed); <-paneGone }
func muxServer(paneGone, serverGone chan int)      { <-paneGone; close(serverGone) }

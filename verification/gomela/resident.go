package completion

// ResidentTurns leaves the worker alive across requests, but each provider group
// must be registered before release and retired before its turn capacity returns.
func ResidentTurns() {
	request := make(chan int)
	registered := make(chan int)
	released := make(chan int)
	retired := make(chan int)
	stop := make(chan int)
	done := make(chan int)
	go residentWorker(request, registered, released, retired, stop, done)
	request <- 1
	<-registered
	released <- 1
	<-retired
	request <- 2
	<-registered
	released <- 1
	<-retired
	stop <- 1
	<-done
}
func residentWorker(request, registered, released, retired, stop, done chan int) {
	<-request
	registered <- 1
	<-released
	retired <- 1
	<-request
	registered <- 1
	<-released
	retired <- 1
	<-stop
	done <- 1
}

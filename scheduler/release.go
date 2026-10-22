package scheduler

// CanRelease is the identity boundary used by durable lease release. PID reuse
// cannot authorize another process to release the old process's reservation.
// @ ensures ok == (pid > 0 && started > 0 && pid == holderPID && started == holderStarted)
func CanRelease(pid, started, holderPID, holderStarted int) (ok bool) {
	return pid > 0 && started > 0 && pid == holderPID && started == holderStarted
}

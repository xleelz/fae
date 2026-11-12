package scheduler

// OwnerForPID measures an authenticated host process's birth identity. Callers
// must obtain pid from kernel peer credentials, never from an untrusted request.
func OwnerForPID(pid int) (Owner, error) { return processOwner(pid) }

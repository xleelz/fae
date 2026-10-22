package daemon

// CanReleaseDependent is shared by scheduling and graph readiness reporting.
// @ ensures res == (!blocking || prerequisiteDone)
func CanReleaseDependent(blocking, prerequisiteDone bool) (res bool) {
	return !blocking || prerequisiteDone
}

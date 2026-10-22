package controller

// MayReleaseOverlap never times out protected work or an unknown query result.
// @ ensures ok == (acknowledged || (expired && !criticalTask && !topNearLimit && !queryFailed))
// @ ensures ok ==> acknowledged || (!criticalTask && !topNearLimit && !queryFailed)
func MayReleaseOverlap(acknowledged, expired, criticalTask, topNearLimit, queryFailed bool) (ok bool) {
	return acknowledged || (expired && !criticalTask && !topNearLimit && !queryFailed)
}

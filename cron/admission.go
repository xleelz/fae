package cron

// CanAdmit requires an active, due recurrence within its end and a pinned template.
// @ ensures ok == (active && due && beforeEnd && pinned)
// @ ensures ok ==> beforeEnd && pinned
func CanAdmit(active, due, beforeEnd, pinned bool) (ok bool) {
	return active && due && beforeEnd && pinned
}

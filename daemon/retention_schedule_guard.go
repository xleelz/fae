package daemon

// @ ensures result == (active && due && authorized)
func CanRunRetentionSchedule(active, due, authorized bool) (result bool) {
	return active && due && authorized
}

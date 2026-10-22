package daemon

// @ ensures result == (active && due && authorized)
func CanRunBackupSchedule(active, due, authorized bool) (result bool) {
	return active && due && authorized
}

package daemon

// CanRunReplicationSchedule keeps recurring transfers behind fresh authority.
// @ ensures result == (active && due && authorized)
func CanRunReplicationSchedule(active, due, authorized bool) (result bool) {
	return active && due && authorized
}

package daemon

// CanDriveDisposition is the FAE-ROB-042 resume floor: a non-terminal row is
// driven only with the disposition persisted at submission (a known mode);
// a row lacking it fails closed rather than defaulting to Local.
// @ ensures result == (terminal || local || normal)
func CanDriveDisposition(terminal, local, normal bool) (result bool) {
	return terminal || local || normal
}

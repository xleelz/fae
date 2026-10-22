package daemon

// @ ensures result == (bytesFit && countFit)
func CanAdmitBackupStorage(bytesFit, countFit bool) (result bool) { return bytesFit && countFit }

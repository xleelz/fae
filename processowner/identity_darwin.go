package processowner

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func birth(pid int) (string, error) {
	p, e := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if e != nil {
		return "", e
	}
	if p == nil || p.Proc.P_pid != int32(pid) {
		return "", fmt.Errorf("process absent")
	}
	return fmt.Sprintf("%d.%06d", p.Proc.P_starttime.Sec, p.Proc.P_starttime.Usec), nil
}

func exited(pid int) (bool, error) {
	p, e := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if e != nil {
		return false, e
	}
	if p == nil || p.Proc.P_pid != int32(pid) {
		return false, fmt.Errorf("process absent")
	}
	return p.Proc.P_stat == 5, nil // Darwin SZOMB; direct child is not reaped yet.
}

// awaitExit is unavailable here; Wait polls the child's state instead.
func awaitExit(int) error { return errUnsupportedWait }

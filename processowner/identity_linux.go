package processowner

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

func birth(pid int) (string, error) {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return "", e
	}
	i := strings.LastIndex(string(b), ")")
	if i < 0 {
		return "", fmt.Errorf("invalid proc stat")
	}
	f := strings.Fields(string(b)[i+1:])
	if len(f) < 20 {
		return "", fmt.Errorf("short proc stat")
	}
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return "", e
	}
	return strings.TrimSpace(string(boot)) + ":" + f[19], nil
}

func exited(pid int) (bool, error) {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return false, e
	}
	i := strings.LastIndex(string(b), ")")
	if i < 0 {
		return false, fmt.Errorf("invalid proc stat")
	}
	f := strings.Fields(string(b)[i+1:])
	if len(f) == 0 {
		return false, fmt.Errorf("short proc stat")
	}
	return f[0] == "Z", nil
}

// awaitExit blocks until the direct child pid has exited, leaving it unreaped
// (waitid WNOWAIT), so Wait need not poll its state (FAE-ARC-002).
func awaitExit(pid int) error {
	var info [128]byte // siginfo_t
	for {
		_, _, e := syscall.Syscall6(syscall.SYS_WAITID, 1 /* P_PID */, uintptr(pid), uintptr(unsafe.Pointer(&info[0])), syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		return nil
	}
}

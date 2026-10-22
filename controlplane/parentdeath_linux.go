//go:build linux

package controlplane

import (
	"os/exec"
	"syscall"
)

func configureParentDeath(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

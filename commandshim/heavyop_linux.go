//go:build linux

package commandshim

import (
	"os/exec"
	"syscall"
)

func configureHeavyOpParentDeath(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

//go:build linux

package ui

import (
	"os/exec"
	"syscall"
)

// ConfigureParentDeath gives the child a parent-death signal (FAE-API-033).
func ConfigureParentDeath(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
}

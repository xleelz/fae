//go:build !linux

package controlplane

import "os/exec"

func configureParentDeath(cmd *exec.Cmd) {}

//go:build !linux

package commandshim

import "os/exec"

func configureHeavyOpParentDeath(cmd *exec.Cmd) {}

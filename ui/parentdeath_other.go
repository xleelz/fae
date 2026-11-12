//go:build !linux

package ui

import "os/exec"

// ConfigureParentDeath relies on the owned-process watchdog and the child's
// own parent check where the platform has no parent-death signal.
func ConfigureParentDeath(cmd *exec.Cmd) {}

//go:build (!darwin && !linux) || (darwin && !cgo)

package controller

import "errors"

func Unsandboxed(pid int) (bool, error) {
	return false, errors.New("native sandbox probe unsupported; refusing launch")
}

func sandboxCondition(int) string { return "no native sandbox probe on this platform" }

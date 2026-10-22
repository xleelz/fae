//go:build linux

package controller

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func Unsandboxed(pid int) (bool, error) {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if e != nil {
		return false, e
	}
	seen := 0
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && (fields[0] == "Seccomp:" || fields[0] == "NoNewPrivs:") {
			value, e := strconv.Atoi(fields[1])
			if e != nil {
				return false, e
			}
			if value != 0 {
				return false, nil
			}
			seen++
		}
	}
	return seen == 2, nil
}

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

// sandboxCondition names the inherited Linux sandbox of pid.
func sandboxCondition(pid int) string {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if e != nil {
		return "sandbox state unreadable: " + e.Error()
	}
	var found []string
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) != 2 || fields[1] == "0":
		case fields[0] == "Seccomp:":
			found = append(found, "seccomp filter mode "+fields[1])
		case fields[0] == "NoNewPrivs:":
			found = append(found, "no-new-privileges set")
		}
	}
	if len(found) == 0 {
		return "seccomp and no-new-privileges state not proven clear"
	}
	return strings.Join(found, ", ")
}

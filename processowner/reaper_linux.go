package processowner

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const prSetChildSubreaper = 36

func setSubreaper() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); e != 0 {
		return e
	}
	return nil
}

// directChildren reads this process's children from every thread's
// /proc/self/task/*/children list (a census fallback where it is missing).
func directChildren() ([]Process, error) {
	tasks, err := filepath.Glob("/proc/self/task/*/children")
	if err != nil || len(tasks) == 0 {
		return censusChildren()
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	page := int64(os.Getpagesize())
	var out []Process
	for _, t := range tasks {
		raw, err := os.ReadFile(t)
		if err != nil {
			continue // the thread exited
		}
		for _, f := range strings.Fields(string(raw)) {
			if _, err := strconv.Atoi(f); err != nil {
				continue
			}
			stat, err := os.ReadFile(filepath.Join("/proc", f, "stat"))
			if err != nil {
				continue // reaped meanwhile
			}
			p, err := parseStat(string(stat), page)
			if err != nil {
				return nil, err
			}
			p.Start = strings.TrimSpace(string(boot)) + ":" + p.Start
			out = append(out, p)
		}
	}
	return out, nil
}

// DescendantPIDs lists root's descendants by walking /proc/<pid>/task/*/children:
// a cheap read of only the instance's own tree (FAE-ARC-002 idle checks).
func DescendantPIDs(root int) ([]int, error) {
	var out []int
	queue := []int{root}
	for len(queue) > 0 && len(out) < 100000 {
		pid := queue[0]
		queue = queue[1:]
		tasks, err := filepath.Glob(filepath.Join("/proc", strconv.Itoa(pid), "task", "*", "children"))
		if err != nil {
			return nil, err
		}
		if pid == root && len(tasks) == 0 {
			return nil, errors.New("process children lists unavailable")
		}
		for _, t := range tasks {
			raw, err := os.ReadFile(t)
			if err != nil {
				continue
			}
			for _, f := range strings.Fields(string(raw)) {
				if child, err := strconv.Atoi(f); err == nil {
					out = append(out, child)
					queue = append(queue, child)
				}
			}
		}
	}
	return out, nil
}

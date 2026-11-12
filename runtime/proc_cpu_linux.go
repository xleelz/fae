//go:build linux

package runtime

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"
)

// userHZ is the fixed clock-tick unit of the /proc userspace ABI.
const userHZ = 100

// procCPURows reads every process's CPU ticks. Processes that exit between
// listing and reading are skipped; a malformed stat record makes the census
// unknown rather than undercounting live work.
func procCPURows() ([]cpuRow, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	rows := []cpuRow{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		parent, ticks, ok := parseProcStat(raw)
		if !ok {
			return nil, false
		}
		rows = append(rows, cpuRow{pid, parent, time.Duration(ticks) * time.Second / userHZ})
	}
	return rows, true
}

// parseProcStat returns the parent PID and utime+stime+cutime+cstime ticks.
// The command name may contain spaces or parentheses, so fields are counted
// from the last closing parenthesis.
func parseProcStat(raw []byte) (int, int64, bool) {
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := strings.Fields(string(raw[i+1:]))
	if len(f) < 15 {
		return 0, 0, false
	}
	parent, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, 0, false
	}
	var ticks int64
	for _, v := range f[11:15] {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false
		}
		ticks += n
	}
	return parent, ticks, true
}

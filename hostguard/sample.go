package hostguard

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Observe reads host telemetry without substituting invented values. Missing
// platform counters remain Unknown and are surfaced as an observability gap.
func Observe(ctx context.Context) (Sample, error) {
	var s Sample
	switch runtime.GOOS {
	case "linux":
		raw, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return s, err
		}
		s.Memory, s.Swap = memoryInfo(string(raw))
		if b, e := os.ReadFile("/proc/loadavg"); e == nil {
			fields := strings.Fields(string(b))
			if len(fields) > 0 {
				s.LoadPerCore = parseReading(fields[0], 1/float64(runtime.NumCPU()))
			}
		}
		if b, e := os.ReadFile("/proc/pressure/io"); e == nil {
			s.IO10, s.IO60 = pressure(string(b))
		}
		if b, e := os.ReadFile("/proc/pressure/memory"); e == nil {
			s.Memory10, s.Memory60 = pressure(string(b))
		}
	case "darwin":
		b, err := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output()
		if err != nil {
			return s, err
		}
		s.Memory = vmMemory(string(b))
		if b, e := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "vm.swapusage").Output(); e == nil {
			f := strings.Fields(string(b))
			for i, v := range f {
				if v == "free" && i+2 < len(f) {
					s.Swap = scaled(f[i+2])
				}
			}
		}
		if b, e := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "vm.loadavg").Output(); e == nil {
			f := strings.Fields(string(b))
			if len(f) > 1 {
				s.LoadPerCore = parseReading(f[1], 1/float64(runtime.NumCPU()))
			}
		}
	default:
		return s, fmt.Errorf("host telemetry unsupported on %s", runtime.GOOS)
	}
	if !s.Memory.Known {
		return s, fmt.Errorf("available memory telemetry unavailable")
	}
	return s, nil
}
func parseReading(s string, scale float64) Reading {
	v, e := strconv.ParseFloat(strings.Trim(s, ".{}"), 64)
	if e != nil || v < 0 {
		return Reading{}
	}
	return Reading{v * scale, true}
}
func memoryInfo(raw string) (Reading, Reading) {
	var memory, swap Reading
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "MemAvailable:":
			memory = parseReading(f[1], 1024)
		case "SwapFree:":
			swap = parseReading(f[1], 1024)
		}
	}
	return memory, swap
}
func pressure(raw string) (Reading, Reading) {
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "some" {
			continue
		}
		return parseReading(strings.TrimPrefix(f[1], "avg10="), 1), parseReading(strings.TrimPrefix(f[2], "avg60="), 1)
	}
	return Reading{}, Reading{}
}
func scaled(s string) Reading {
	if len(s) < 2 {
		return Reading{}
	}
	scale := map[byte]float64{'K': 1024, 'M': 1024 * 1024, 'G': 1024 * 1024 * 1024}[s[len(s)-1]]
	if scale == 0 {
		return Reading{}
	}
	return parseReading(s[:len(s)-1], scale)
}
func vmMemory(raw string) Reading {
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 {
		return Reading{}
	}
	page := float64(0)
	f := strings.Fields(lines[0])
	for i, v := range f {
		if v == "size" && i+2 < len(f) {
			index := i + 1
			if f[index] == "of" {
				index++
			}
			page = parseReading(f[index], 1).Value
		}
	}
	if page <= 0 {
		return Reading{}
	}
	total := float64(0)
	count := 0
	for _, line := range lines[1:] {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if k == "Pages free" || k == "Pages inactive" || k == "Pages speculative" {
			r := parseReading(strings.TrimSpace(v), page)
			if !r.Known {
				return Reading{}
			}
			total += r.Value
			count++
		}
	}
	return Reading{total, count == 3}
}

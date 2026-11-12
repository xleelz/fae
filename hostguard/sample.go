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
// platform counters remain Unknown and are surfaced as an observability gap;
// unreadable memory available is such a gap, on which the hard breaker
// proceeds and records the uncertainty (FAE-HOST-004).
func Observe(ctx context.Context) (Sample, error) {
	var s Sample
	switch runtime.GOOS {
	case "linux":
		return observeLinux(os.ReadFile, runtime.NumCPU()), nil
	case "darwin":
		if b, e := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output(); e == nil {
			s.Memory = vmMemory(string(b))
		}
		if b, e := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "vm.swapusage").Output(); e == nil {
			s.Swap = swapUsage(string(b))
		}
		if b, e := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "vm.loadavg").Output(); e == nil {
			f := strings.Fields(string(b))
			if len(f) > 1 {
				s.LoadPerCore = parseReading(f[1], 1/float64(runtime.NumCPU()))
			}
		}
		return s, nil
	}
	return s, fmt.Errorf("host telemetry unsupported on %s", runtime.GOOS)
}

// observeLinux reads /proc through read. An unreadable file or field stays
// Unknown.
func observeLinux(read func(string) ([]byte, error), cpus int) Sample {
	var s Sample
	if raw, e := read("/proc/meminfo"); e == nil {
		s.Memory, s.Swap = memoryInfo(string(raw))
	}
	if b, e := read("/proc/loadavg"); e == nil && cpus > 0 {
		fields := strings.Fields(string(b))
		if len(fields) > 0 {
			s.LoadPerCore = parseReading(fields[0], 1/float64(cpus))
		}
	}
	if b, e := read("/proc/pressure/io"); e == nil {
		s.IO10, s.IO60 = pressure(string(b))
	}
	if b, e := read("/proc/pressure/memory"); e == nil {
		s.Memory10, s.Memory60 = pressure(string(b))
	}
	return s
}
func parseReading(s string, scale float64) Reading {
	v, e := strconv.ParseFloat(strings.Trim(s, ".{}"), 64)
	if e != nil || v < 0 {
		return Reading{}
	}
	return Reading{v * scale, true}
}

// memoryInfo reads MemAvailable and SwapTotal from /proc/meminfo. Swap total,
// not free, selects the no-swap hard floor (FAE-HOST-001, -003).
func memoryInfo(raw string) (memory, swapTotal Reading) {
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "MemAvailable:":
			memory = parseReading(f[1], 1024)
		case "SwapTotal:":
			swapTotal = parseReading(f[1], 1024)
		}
	}
	return memory, swapTotal
}

// pressure reads the PSI "full" line (FAE-HOST-001): the share of time in which
// all non-idle tasks stalled on the resource. A missing "full" line or field
// stays Unknown, a recorded gap; it is never substituted from the "some" line.
func pressure(raw string) (avg10, avg60 Reading) {
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "full" {
			continue
		}
		for _, field := range f[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch key {
			case "avg10":
				avg10 = parseReading(value, 1)
			case "avg60":
				avg60 = parseReading(value, 1)
			}
		}
		return avg10, avg60
	}
	return Reading{}, Reading{}
}

// swapUsage reads swap total from macOS `sysctl -n vm.swapusage`, e.g.
// "total = 2048.00M  used = 1024.00M  free = 1024.00M  (encrypted)".
func swapUsage(raw string) Reading {
	f := strings.Fields(raw)
	for i, v := range f {
		if v == "total" && i+2 < len(f) && f[i+1] == "=" {
			return scaled(f[i+2])
		}
	}
	return Reading{}
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

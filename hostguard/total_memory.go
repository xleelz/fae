package hostguard

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// TotalMemory measures physical memory only for impossible-footprint rejection.
// Missing observations never substitute available memory or invented capacity.
func TotalMemory(ctx context.Context) (int64, error) {
	switch runtime.GOOS {
	case "linux":
		raw, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			var kib int64
			if _, err := fmt.Sscanf(line, "MemTotal: %d kB", &kib); err == nil && kib > 0 && kib <= math.MaxInt64/1024 {
				return kib * 1024, nil
			}
		}
	case "darwin":
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		raw, err := exec.CommandContext(bounded, "/usr/sbin/sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0, err
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err == nil && n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("physical memory telemetry unavailable")
}

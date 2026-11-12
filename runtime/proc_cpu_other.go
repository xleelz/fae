//go:build !linux

package runtime

// procCPURows has no /proc source here; processCPU falls back to ps.
func procCPURows() ([]cpuRow, bool) { return nil, false }

package hostguard

import "testing"

func TestTelemetryParsing(t *testing.T) {
	m, s := memoryInfo("MemAvailable: 123 kB\nSwapFree: 456 kB\n")
	if m.Value != 123*1024 || s.Value != 456*1024 || !m.Known {
		t.Fatal(m, s)
	}
	a, b := pressure("some avg10=1.25 avg60=2.50 avg300=0 total=1")
	if a.Value != 1.25 || b.Value != 2.5 {
		t.Fatal(a, b)
	}
	m = vmMemory("Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free: 2.\nPages inactive: 3.\nPages speculative: 4.\n")
	if !m.Known || m.Value != 9*16384 {
		t.Fatal(m)
	}
	if vmMemory("bad").Known || scaled("bad").Known || parseReading("-1", 1).Known {
		t.Fatal("invalid telemetry accepted")
	}
}

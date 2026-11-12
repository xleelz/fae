package hostguard

import (
	"errors"
	"testing"
)

func TestTelemetryParsing(t *testing.T) {
	m, s := memoryInfo("MemAvailable: 123 kB\nSwapTotal: 789 kB\nSwapFree: 456 kB\n")
	if m.Value != 123*1024 || s.Value != 789*1024 || !m.Known || !s.Known {
		t.Fatal(m, s)
	}
	a, b := pressure("some avg10=9.00 avg60=9.00 avg300=0 total=1\nfull avg10=1.25 avg60=2.50 avg300=0 total=1")
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

// Literal Linux /proc content: both PSI lines present, the "full" line must be
// selected (FAE-HOST-001), and swap total, not swap free, is reported.
func TestProcPressureSelectsFullLine(t *testing.T) {
	const io = "some avg10=42.10 avg60=30.05 avg300=12.00 total=987654321\n" +
		"full avg10=4.75 avg60=2.50 avg300=1.00 total=123456789\n"
	const memory = "some avg10=7.00 avg60=6.00 avg300=5.00 total=1000\n" +
		"full avg10=0.00 avg60=0.13 avg300=0.10 total=900\n"
	a10, a60 := pressure(io)
	if !a10.Known || !a60.Known || a10.Value != 4.75 || a60.Value != 2.5 {
		t.Fatal("io full", a10, a60)
	}
	m10, m60 := pressure(memory)
	if !m10.Known || !m60.Known || m10.Value != 0 || m60.Value != 0.13 {
		t.Fatal("memory full", m10, m60)
	}
	// Line order does not matter; the "some" values are never used.
	b10, b60 := pressure("full avg10=1.00 avg60=2.00 avg300=3.00 total=4\nsome avg10=50.00 avg60=60.00 avg300=70.00 total=80\n")
	if b10.Value != 1 || b60.Value != 2 {
		t.Fatal("order", b10, b60)
	}
}

// A kernel or platform without a "full" line yields Unknown readings: a
// recorded gap, which the soft guard and the I/O gate treat as proceed-with-gap
// (FAE-HOST-004), never as pressure and never as an invented zero.
func TestProcPressureMissingFullIsUnknownGap(t *testing.T) {
	for _, raw := range []string{
		"some avg10=0.50 avg60=0.40 avg300=0.30 total=10\n",
		"",
		"full avg10=bogus avg60=-1 avg300=0 total=1\n",
	} {
		a10, a60 := pressure(raw)
		if a10.Known || a60.Known {
			t.Fatalf("%q produced %v %v", raw, a10, a60)
		}
		s := Sample{Memory: Reading{20 * float64(GiB), true}, Swap: Reading{float64(GiB), true}, IO10: a10, IO60: a60, Memory10: a10, Memory60: a60, LoadPerCore: Reading{0.5, true}}
		if d := Admit(s, DefaultConfig(), 0, true); !d.Allowed || !d.Gap {
			t.Fatalf("%q: missing PSI must proceed with a gap: %+v", raw, d)
		}
		gate, err := NewIOGate(DefaultPolicy())
		if err != nil {
			t.Fatal(err)
		}
		step := gate.Record(epoch, s, DefaultConfig())
		if step.Outcome != OutcomeProceed || !step.Gap || step.Level != LevelUnknown || step.Reason != "TelemetryGap" {
			t.Fatalf("%q: gate %+v", raw, step)
		}
	}
	// A "full" line missing only avg60 keeps avg10.
	a10, a60 := pressure("full avg10=3.00 total=1\n")
	if !a10.Known || a10.Value != 3 || a60.Known {
		t.Fatal(a10, a60)
	}
}

func TestProcMeminfoSwapTotal(t *testing.T) {
	const withSwap = "MemTotal:       65843232 kB\nMemFree:         1234567 kB\nMemAvailable:   63282584 kB\nSwapCached:            0 kB\nSwapTotal:       2097148 kB\nSwapFree:              0 kB\n"
	m, s := memoryInfo(withSwap)
	if !m.Known || m.Value != 63282584*1024 || !s.Known || s.Value != 2097148*1024 {
		t.Fatal("exhausted but configured swap must report its total", m, s)
	}
	const noSwap = "MemTotal:       65843232 kB\nMemAvailable:    3145728 kB\nSwapTotal:             0 kB\nSwapFree:              0 kB\n"
	m, s = memoryInfo(noSwap)
	if !s.Known || s.Value != 0 {
		t.Fatal("no swap", s)
	}
	// 3 GiB available: below the 4 GiB no-swap hard floor, above the 2 GiB floor.
	sample := Sample{Memory: m, Swap: s}
	if d := Admit(sample, DefaultConfig(), 0, true); !d.Hard {
		t.Fatal("no swap configured must use the no-swap hard floor", d)
	}
	_, sample.Swap = memoryInfo(withSwap)
	if d := Admit(sample, DefaultConfig(), 0, true); d.Hard || d.Reason != "MemoryReservation" && d.Reason != "HostPressure" {
		t.Fatal("configured swap uses the ordinary hard floor; soft admission still refuses", d)
	}
	_, missing := memoryInfo("MemAvailable: 3145728 kB\n")
	if missing.Known {
		t.Fatal("missing SwapTotal must stay unknown")
	}
	sample.Swap = missing
	if d := Admit(sample, DefaultConfig(), 0, true); !d.Hard {
		t.Fatal("unreadable swap must use the conservative no-swap floor", d)
	}
}

func TestDarwinSwapUsageTotal(t *testing.T) {
	r := swapUsage("total = 2048.00M  used = 1536.00M  free = 512.00M  (encrypted)\n")
	if !r.Known || r.Value != 2048*1024*1024 {
		t.Fatal(r)
	}
	if r = swapUsage("total = 0.00M  used = 0.00M  free = 0.00M  (encrypted)\n"); !r.Known || r.Value != 0 {
		t.Fatal(r)
	}
	if swapUsage("garbage").Known {
		t.Fatal("garbage accepted")
	}
}

// FAE-HOST-004: unreadable memory available is an observability gap, not an
// error. The hard breaker proceeds and records the uncertainty; readable
// pressure still applies.
func TestLinuxUnreadableMemoryAvailableProceedsWithGap(t *testing.T) {
	proc := map[string]string{
		"/proc/meminfo":         "MemTotal: 65843232 kB\nSwapTotal: 0 kB\n",
		"/proc/loadavg":         "0.50 0.40 0.30 1/100 1\n",
		"/proc/pressure/io":     "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=1.00 avg60=1.00 avg300=0.00 total=0\n",
		"/proc/pressure/memory": "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
	}
	read := func(path string) ([]byte, error) {
		if v, ok := proc[path]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("unreadable")
	}
	s := observeLinux(read, 4)
	if s.Memory.Known || !s.Swap.Known || !s.IO10.Known || !s.LoadPerCore.Known {
		t.Fatalf("%+v", s)
	}
	if d := Admit(s, DefaultConfig(), 0, true); !d.Allowed || d.Hard || !d.Gap {
		t.Fatal("unreadable memory must proceed with a gap", d)
	}
	s.IO10.Value = 50
	if d := Admit(s, DefaultConfig(), 0, true); d.Allowed || d.Reason != "HostPressure" {
		t.Fatal("readable pressure ignored", d)
	}
	delete(proc, "/proc/meminfo")
	if s = observeLinux(read, 4); s.Memory.Known || s.Swap.Known || !s.IO10.Known {
		t.Fatalf("%+v", s)
	}
	if missing := Missing(s); len(missing) != 2 || missing[0] != "memory_available" || missing[1] != "swap_total" {
		t.Fatal(missing)
	}
}

// Package hostguard implements pure admission decisions over injected telemetry.
package hostguard

import (
	"math"
	"time"
)

const GiB int64 = 1 << 30

type Reading struct {
	Value float64
	Known bool
}
type Sample struct {
	Memory, Swap                                Reading
	IO10, IO60, Memory10, Memory60, LoadPerCore Reading
	LivePanes                                   int
}
type Config struct {
	Soft, Hard, HardNoSwap, PerPane int64
	IO, MemoryPressure, Load        float64
	PaneLimit                       int
}

func DefaultConfig() Config { return Config{6 * GiB, 2 * GiB, 4 * GiB, 7 * GiB / 10, 10, 5, 4, 60} }

type Decision struct {
	Allowed, Hard, Gap bool
	Reason             string
}

func known(r Reading) bool {
	return r.Known && !math.IsNaN(r.Value) && !math.IsInf(r.Value, 0) && r.Value >= 0
}
func Admit(s Sample, c Config, pending int, ledgerReadable bool) Decision {
	return admit(s, c, pending, ledgerReadable, true)
}

// AdmitResidentTurn checks host pressure for an already reserved resident pane.
// New resident creation still requires the full pane/memory admission path.
func AdmitResidentTurn(s Sample, c Config, ledgerReadable bool) Decision {
	return admit(s, c, 0, ledgerReadable, false)
}

func admit(s Sample, c Config, pending int, ledgerReadable, newPane bool) Decision {
	if !ledgerReadable {
		return Decision{Reason: "LeaseLedger"}
	}
	if !known(Reading{c.IO, true}) || !known(Reading{c.MemoryPressure, true}) || !known(Reading{c.Load, true}) || pending < 0 || c.Hard < 0 || c.HardNoSwap < 0 || c.Soft < 0 || c.PerPane <= 0 || c.PaneLimit <= 0 {
		return Decision{Reason: "Configuration"}
	}
	gap := !known(s.Memory) || !known(s.Swap) || !known(s.IO10) || !known(s.IO60) || !known(s.Memory10) || !known(s.Memory60) || !known(s.LoadPerCore)
	floor := c.Hard
	if !known(s.Swap) || s.Swap.Value == 0 {
		floor = c.HardNoSwap
	}
	if known(s.Memory) && s.Memory.Value < float64(floor) {
		return Decision{Hard: true, Gap: gap, Reason: "HostHardLimit"}
	}
	if newPane && s.LivePanes >= c.PaneLimit {
		return Decision{Gap: gap, Reason: "PaneLimit"}
	}
	if newPane && known(s.Memory) && float64(pending)+1 > (s.Memory.Value-float64(floor))/float64(c.PerPane) {
		return Decision{Gap: gap, Reason: "MemoryReservation"}
	}
	if (known(s.Memory) && s.Memory.Value < float64(c.Soft)) || (known(s.IO10) && s.IO10.Value > c.IO) || (known(s.Memory10) && s.Memory10.Value > c.MemoryPressure) || (known(s.LoadPerCore) && s.LoadPerCore.Value > c.Load) {
		return Decision{Gap: gap, Reason: "HostPressure"}
	}
	return Decision{Allowed: true, Gap: gap}
}

// PressureClear enforces strict below-threshold readings separated by 30s.
func PressureClear(first, second Reading, t1, t2 time.Time, threshold float64) bool {
	return known(first) && known(second) && first.Value < threshold && second.Value < threshold && t2.Sub(t1) >= 30*time.Second
}

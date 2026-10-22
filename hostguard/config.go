package hostguard

// ValidConfig rejects internally inconsistent or unbounded admission settings.
func ValidConfig(c Config) bool {
	return c.Hard >= 0 && c.HardNoSwap >= c.Hard && c.Soft >= c.HardNoSwap && c.Soft <= 1<<60 && c.PerPane > 0 && c.PerPane <= 1<<60 && known(Reading{c.IO, true}) && known(Reading{c.MemoryPressure, true}) && known(Reading{c.Load, true}) && c.IO <= 100 && c.MemoryPressure <= 100 && c.Load <= 1000000 && c.PaneLimit >= 1 && c.PaneLimit <= 1000000
}

// Conservative combines shared host policy: memory floors rise, pressure ceilings fall.
func Conservative(a, b Config) Config {
	a.Soft = max(a.Soft, b.Soft)
	a.Hard = max(a.Hard, b.Hard)
	a.HardNoSwap = max(a.HardNoSwap, b.HardNoSwap)
	a.PerPane = max(a.PerPane, b.PerPane)
	a.IO = min(a.IO, b.IO)
	a.MemoryPressure = min(a.MemoryPressure, b.MemoryPressure)
	a.Load = min(a.Load, b.Load)
	a.PaneLimit = min(a.PaneLimit, b.PaneLimit)
	return a
}

package hostguard

import (
	"math"
	"testing"
)

func TestDynamicThresholdValidationAndMerge(t *testing.T) {
	a := DefaultConfig()
	if !ValidConfig(a) {
		t.Fatal(a)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.Hard = -1 }, func(c *Config) { c.HardNoSwap = c.Hard - 1 }, func(c *Config) { c.Soft = c.HardNoSwap - 1 }, func(c *Config) { c.PerPane = 0 }, func(c *Config) { c.IO = math.NaN() }, func(c *Config) { c.MemoryPressure = 101 }, func(c *Config) { c.Load = math.Inf(1) }} {
		c := a
		mutate(&c)
		if ValidConfig(c) {
			t.Fatal(c)
		}
	}
	b := a
	b.Load = 1
	b.Soft += GiB
	b.PerPane += GiB
	c := Conservative(a, b)
	if !ValidConfig(c) || c.Load != 1 || c.Soft != b.Soft || c.PerPane != b.PerPane {
		t.Fatal(c)
	}
}

package hostguard

import (
	"math"
	"testing"
	"time"
)

func healthy() Sample {
	return Sample{Reading{20 * float64(GiB), true}, Reading{float64(GiB), true}, Reading{0, true}, Reading{0, true}, Reading{0, true}, Reading{0, true}, Reading{0, true}, 0}
}
func TestThresholds(t *testing.T) {
	c := DefaultConfig()
	for _, tc := range []struct {
		name      string
		set       func(*Sample, float64)
		threshold float64
	}{{"io", func(s *Sample, v float64) { s.IO10.Value = v }, c.IO}, {"memory pressure", func(s *Sample, v float64) { s.Memory10.Value = v }, c.MemoryPressure}, {"load", func(s *Sample, v float64) { s.LoadPerCore.Value = v }, c.Load}} {
		s := healthy()
		tc.set(&s, tc.threshold)
		if !Admit(s, c, 0, true).Allowed {
			t.Fatal(tc.name)
		}
		tc.set(&s, tc.threshold+.001)
		if Admit(s, c, 0, true).Allowed {
			t.Fatal(tc.name)
		}
	}
	s := healthy()
	s.Memory.Value = float64(c.Soft)
	if !Admit(s, c, 0, true).Allowed {
		t.Fatal("floor")
	}
	s.Memory.Value--
	if Admit(s, c, 0, true).Allowed {
		t.Fatal("below floor")
	}
	s = healthy()
	s.LivePanes = c.PaneLimit
	if Admit(s, c, 0, true).Reason != "PaneLimit" {
		t.Fatal("panes")
	}
}
func TestHardReservationAndGaps(t *testing.T) {
	c := DefaultConfig()
	s := healthy()
	s.Memory.Value = float64(3 * GiB)
	if Admit(s, c, 0, true).Hard {
		t.Fatal("swap")
	}
	s.Swap.Value = 0
	if !Admit(s, c, 0, true).Hard {
		t.Fatal("no swap")
	}
	s = healthy()
	if Admit(s, c, 30, true).Reason != "MemoryReservation" {
		t.Fatal("reservation")
	}
	s.Memory.Known = false
	s.IO10.Value = math.NaN()
	d := Admit(s, c, 30, true)
	if !d.Allowed || !d.Gap {
		t.Fatal(d)
	}
	if Admit(s, c, 0, false).Allowed {
		t.Fatal("ledger")
	}
}
func TestClearNeedsTwoReadings(t *testing.T) {
	now := time.Now()
	r := Reading{4, true}
	if PressureClear(r, r, now, now.Add(29*time.Second), 5) {
		t.Fatal("too early")
	}
	if !PressureClear(r, r, now, now.Add(30*time.Second), 5) {
		t.Fatal("clear")
	}
	if PressureClear(r, Reading{5, true}, now, now.Add(time.Minute), 5) || PressureClear(r, Reading{}, now, now.Add(time.Minute), 5) {
		t.Fatal("not clear")
	}
}
func TestBadConfiguration(t *testing.T) {
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		c := DefaultConfig()
		c.IO = bad
		if Admit(healthy(), c, 0, true).Allowed {
			t.Fatal(bad)
		}
	}
}

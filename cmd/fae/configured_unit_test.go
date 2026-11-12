package main

import (
	"math"
	"testing"

	"github.com/xleelz/fae/hostguard"
)

// FAE-HOST-007: the landing-floor conversion is bounded. A floor that is not
// finite, not above zero, too large or below one byte is refused, so it can
// never overflow int64 or wrap to the negative floor that applies only the
// soft floor; every accepted floor converts to a positive byte count.
func TestLandingFloorBytesIsBounded(t *testing.T) {
	for _, gib := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e19, 1e300, maxLandingFloorGiB * 2, -5, 0, math.Copysign(0, -1), 1e-300} {
		if b, ok := landingFloorBytes(gib); ok || b != 0 {
			t.Errorf("%v GiB: %d bytes, ok %v; want refused", gib, b, ok)
		}
	}
	for gib, want := range map[float64]int64{12: 12 * hostguard.GiB, 0.5: hostguard.GiB / 2, maxLandingFloorGiB: maxLandingFloorGiB * hostguard.GiB} {
		if b, ok := landingFloorBytes(gib); !ok || b != want {
			t.Errorf("%v GiB: %d bytes, ok %v; want %d", gib, b, ok, want)
		}
	}
}

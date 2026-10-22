package kernel

import "testing"

func TestDrainGuardExhaustive(t *testing.T) {
	for _, draining := range []bool{false, true} {
		for active := -1; active < 4; active++ {
			for pending := -1; pending < 4; pending++ {
				got := CanFinishDrain(draining, active, pending)
				if got != (draining && active == 0 && pending == 0) {
					t.Fatal(draining, active, pending, got)
				}
			}
		}
	}
}

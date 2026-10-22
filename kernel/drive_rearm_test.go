package kernel

import "testing"

func TestDriveRearmAllStates(t *testing.T) {
	for bits := 0; bits < 16; bits++ {
		a, p, b, r := bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0
		if CanRearmDrive(a, p, b, r) != (!a && !p && b && r) {
			t.Fatal(bits)
		}
	}
}

package kernel

import "testing"

func TestCanApplyReconcileExhaustive(t *testing.T) {
	for bits := 0; bits < 16; bits++ {
		got := CanApplyReconcile(bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0)
		if got != (bits == 15) {
			t.Fatalf("bits %d admitted %v", bits, got)
		}
	}
}

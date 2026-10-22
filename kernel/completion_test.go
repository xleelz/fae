package kernel

import "testing"

func TestCanCompleteExhaustive(t *testing.T) {
	for bits := 0; bits < 16; bits++ {
		terminal, pending, token, evidence := bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0
		want := bits == 14
		if got := CanComplete(terminal, pending, token, evidence); got != want {
			t.Errorf("mask %04b: got %v want %v", bits, got, want)
		}
	}
}

package kernel

import "testing"

func TestCanKeepOwnHunkExhaustive(t *testing.T) {
	for bits := 0; bits < 4; bits++ {
		if CanKeepOwnHunk(bits&1 != 0, bits&2 != 0) != (bits == 3) {
			t.Fatalf("lost hunk admitted: %d", bits)
		}
	}
}

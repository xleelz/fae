package kernel

import "testing"

func TestCanRetryIntegrationExhaustive(t *testing.T) {
	for bits := 0; bits < 16; bits++ {
		if CanRetryIntegration(bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0) != (bits == 15) {
			t.Fatalf("unsafe integration retry: %d", bits)
		}
	}
}

package kernel

import "testing"

func TestCanRetryStallAllProofs(t *testing.T) {
	for n := 0; n < 16; n++ {
		if got := CanRetryStall(n&1 != 0, n&2 != 0, n&4 != 0, n&8 != 0); got != (n == 15) {
			t.Fatal(n, got)
		}
	}
}

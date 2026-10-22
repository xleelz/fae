package kernel

import "testing"

func TestStartupProofTruthTable(t *testing.T) {
	for n := 0; n < 8; n++ {
		proof, retired, first := n&1 != 0, n&2 != 0, n&4 != 0
		if CanRetryStartup(proof, retired, first) != (n == 7) {
			t.Fatal(n)
		}
	}
}

package kernel

import "testing"

func TestCanShedIdleExhaustive(t *testing.T) {
	for bits := 0; bits < 16; bits++ {
		for inflight := -1; inflight <= 2; inflight++ {
			global, pre, protected, active := bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0
			got := CanShedIdle(global, pre, protected, active, inflight)
			want := global && pre && !protected && !active && inflight == 0
			if got != want {
				t.Fatalf("bits %d inflight %d: %t", bits, inflight, got)
			}
		}
	}
}

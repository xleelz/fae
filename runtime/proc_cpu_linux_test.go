//go:build linux

package runtime

import "testing"

func TestParseProcStat(t *testing.T) {
	parent, ticks, ok := parseProcStat([]byte("42 (a) b (c) S 7 42 42 0 -1 4194560 100 0 0 0 11 22 3 4 20 0 1 0 5 0 0\n"))
	if !ok || parent != 7 || ticks != 40 {
		t.Fatal(parent, ticks, ok)
	}
	for _, raw := range []string{"42 no-paren S 7", "42 (x) S 7 1 2", "42 (x) S p 1 2 3 4 5 6 7 8 9 1 2 3 4", "42 (x) S 7 1 2 3 4 5 6 7 8 9 1 -2 3 4"} {
		if _, _, ok := parseProcStat([]byte(raw)); ok {
			t.Fatal(raw)
		}
	}
}

package kernel

import "testing"

func TestCanResumeProvider(t *testing.T) {
	for i := 0; i < 8; i++ {
		enabled, scopedPolicy, fresh := i&1 != 0, i&2 != 0, i&4 != 0
		if CanResumeProvider(enabled, scopedPolicy, fresh) != (enabled && scopedPolicy && !fresh) {
			t.Fatal(i)
		}
	}
}
